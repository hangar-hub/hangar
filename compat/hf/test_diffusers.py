"""diffusers loads a tiny pipeline from Hangar (13-testing.md, nightly).

diffusers 0.41 needs huggingface_hub below 2.0, so it cannot share the
locked environment, which has huggingface_hub 2.2.0. The nightly matrix cell
installs it with an older huggingface_hub:

    uv run --frozen --project compat --with diffusers==0.41.0 \\
        --with huggingface_hub==1.33.0 python -m pytest compat/hf/test_diffusers.py

In the locked environment the module is reported as skipped, with this
reason.
"""

import pytest

from test_libraries import NetworkGuard

diffusers = pytest.importorskip(
    "diffusers", reason="diffusers needs huggingface_hub<2; the nightly matrix cell installs it (see module docstring)"
)

pytestmark = pytest.mark.usefixtures("no_xet")


@pytest.mark.endpoint("diffusers from_pretrained")
def test_diffusers_pipeline(api, repo_name, tmp_path, monkeypatch):
    import torch
    from diffusers import DDPMPipeline, DDPMScheduler, DiffusionPipeline, UNet2DModel

    torch.manual_seed(0)
    unet = UNet2DModel(
        sample_size=8,
        in_channels=3,
        out_channels=3,
        layers_per_block=1,
        block_out_channels=(8, 8),
        down_block_types=("DownBlock2D", "DownBlock2D"),
        up_block_types=("UpBlock2D", "UpBlock2D"),
        norm_num_groups=4,
    )
    pipe = DDPMPipeline(unet=unet, scheduler=DDPMScheduler(num_train_timesteps=10))
    pipe.save_pretrained(tmp_path / "pipe")
    repo = api.create_repo(repo_name).repo_id
    # Subfolders (unet/, scheduler/) exercise the recursive siblings diffusers
    # plans its downloads from.
    api.upload_folder(folder_path=tmp_path / "pipe", repo_id=repo)

    guard = NetworkGuard(monkeypatch)
    loaded = DiffusionPipeline.from_pretrained(repo, cache_dir=str(tmp_path / "cache"))
    assert type(loaded).__name__ == "DDPMPipeline"
    want = unet.state_dict()
    got = loaded.unet.state_dict()
    assert want.keys() == got.keys()
    assert all(torch.equal(want[k], got[k]) for k in want)
    images = loaded(num_inference_steps=1, output_type="np", generator=torch.manual_seed(0)).images
    assert images.shape == (1, 8, 8, 3)
    assert guard.attempts == [], guard.attempts
