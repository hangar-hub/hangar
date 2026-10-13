"""Loaders that sit on huggingface_hub: transformers, sentence-transformers
and datasets. Fixtures are tiny models built in the test, so nothing is
downloaded from huggingface.co."""

import ipaddress
import socket
import threading
from pathlib import Path

import pytest

pytestmark = pytest.mark.usefixtures("no_xet")

VOCAB = ["[PAD]", "[UNK]", "[CLS]", "[SEP]", "[MASK]", "hello", "world", "hangar", "model", "##s"]


def _tiny_bert(root: Path):
    from transformers import BertConfig, BertModel, BertTokenizerFast

    root.mkdir(parents=True, exist_ok=True)
    (root / "vocab.txt").write_text("\n".join(VOCAB) + "\n")
    tok = BertTokenizerFast(vocab_file=str(root / "vocab.txt"))
    config = BertConfig(
        vocab_size=len(VOCAB), hidden_size=16, num_hidden_layers=1, num_attention_heads=2,
        intermediate_size=32, max_position_embeddings=64,
    )
    import torch

    torch.manual_seed(0)
    model = BertModel(config)
    model.save_pretrained(root)
    tok.save_pretrained(root)
    return model, tok


@pytest.fixture(scope="module")
def tiny_bert(tmp_path_factory):
    root = tmp_path_factory.mktemp("tiny-bert")
    model, tok = _tiny_bert(root)
    return root, model, tok


@pytest.mark.endpoint("transformers from_pretrained")
def test_transformers_auto_classes(api, repo_name, tiny_bert):
    import torch
    from transformers import AutoModel, AutoTokenizer

    root, model, _ = tiny_bert
    repo = api.create_repo(repo_name).repo_id
    api.upload_folder(folder_path=root, repo_id=repo)
    loaded = AutoModel.from_pretrained(repo)
    tok = AutoTokenizer.from_pretrained(repo)
    ids = tok("hello hangar models", return_tensors="pt")
    with torch.no_grad():
        assert torch.allclose(loaded(**ids).last_hidden_state, model.eval()(**ids).last_hidden_state)


class NetworkGuard:
    """Refuses every connection that is not to the local test server, and
    records the host names asked for."""

    def __init__(self, monkeypatch):
        self.attempts: list[str] = []
        real_getaddrinfo = socket.getaddrinfo
        real_connect = socket.socket.connect

        def getaddrinfo(host, *args, **kwargs):
            if host not in ("127.0.0.1", "localhost", "::1"):
                self.attempts.append(str(host))
                raise socket.gaierror(f"compat/hf blocks outbound network: {host}")
            return real_getaddrinfo(host, *args, **kwargs)

        def connect(sock, address):
            host = address[0] if isinstance(address, tuple) else address
            try:
                if not ipaddress.ip_address(host).is_loopback:
                    self.attempts.append(str(host))
                    raise ConnectionRefusedError(f"compat/hf blocks outbound network: {host}")
            except ValueError:
                pass
            return real_connect(sock, address)

        monkeypatch.setattr(socket, "getaddrinfo", getaddrinfo)
        monkeypatch.setattr(socket.socket, "connect", connect)


@pytest.mark.endpoint("transformers safetensors conversion guard")
def test_pickle_only_model_never_contacts_conversion_space(api, repo_name, tiny_bert, monkeypatch):
    import torch
    from transformers import AutoModel

    root, model, _ = tiny_bert
    pickle_dir = root.parent / "pickle-only"
    pickle_dir.mkdir(exist_ok=True)
    (pickle_dir / "config.json").write_text((root / "config.json").read_text())
    torch.save(model.state_dict(), pickle_dir / "pytorch_model.bin")
    repo = api.create_repo(repo_name).repo_id
    api.upload_folder(folder_path=pickle_dir, repo_id=repo)

    guard = NetworkGuard(monkeypatch)
    monkeypatch.delenv("DISABLE_SAFETENSORS_CONVERSION", raising=False)
    # The background conversion thread asks Hangar for the Commits, gets the
    # guard's 403 and stops before it reaches safetensors-convert.hf.space.
    AutoModel.from_pretrained(repo)
    for t in threading.enumerate():
        if t.name == "Thread-auto_conversion":
            t.join(timeout=60)
    # The synchronous path (use_safetensors=True) fails with Hangar's message.
    with pytest.raises(Exception, match="safetensors conversion service"):
        AutoModel.from_pretrained(repo, use_safetensors=True)
    assert not any("hf.space" in a for a in guard.attempts), guard.attempts
    assert guard.attempts == [], guard.attempts


@pytest.mark.endpoint("sentence-transformers load")
def test_sentence_transformers(api, repo_name, tiny_bert, tmp_path):
    from sentence_transformers import SentenceTransformer, models

    root, _, _ = tiny_bert
    word = models.Transformer(str(root))
    pool = models.Pooling(word.get_word_embedding_dimension())
    st = SentenceTransformer(modules=[word, pool])
    st.save(str(tmp_path / "st"))
    repo = api.create_repo(repo_name).repo_id
    api.upload_folder(folder_path=tmp_path / "st", repo_id=repo)
    loaded = SentenceTransformer(repo)
    a = loaded.encode(["hello world"])
    b = st.encode(["hello world"])
    assert a.shape == b.shape == (1, 16)
    assert abs(a - b).max() < 1e-5


@pytest.mark.endpoint("datasets load_dataset", "HfFileSystem")
def test_datasets_load_dataset(api, repo_name, tmp_path):
    from datasets import load_dataset

    repo = api.create_repo(repo_name, repo_type="dataset").repo_id
    data = tmp_path / "data"
    (data / "data").mkdir(parents=True)
    (data / "data" / "train.csv").write_text("text,label\nhello,1\nworld,0\nhangar,1\n")
    (data / "data" / "test.csv").write_text("text,label\nmodel,0\n")
    (data / "README.md").write_text("---\nlicense: mit\n---\n# Tiny rows\n")
    api.upload_folder(folder_path=data, repo_id=repo, repo_type="dataset")

    ds = load_dataset(repo, cache_dir=str(tmp_path / "cache"))
    assert ds["train"].num_rows == 3
    assert ds["test"][0] == {"text": "model", "label": 0}
    streamed = load_dataset(repo, split="train", streaming=True)
    assert [r["text"] for r in streamed] == ["hello", "world", "hangar"]
