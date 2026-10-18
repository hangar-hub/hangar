package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hangar-hub/hangar/internal/index/card"
	"github.com/hangar-hub/hangar/internal/index/gguf"
	"github.com/hangar-hub/hangar/internal/index/safetensors"
	"github.com/hangar-hub/hangar/internal/model"
)

// Version is the indexer version. Results are keyed by it; bumping it
// re-indexes every branch head in the background. Version 2 adds the
// datasets a model card lists.
const Version = 2

// Limits on small files the indexer reads whole.
const (
	maxJSONFile    = 8 << 20
	maxReadme      = 4 << 20
	maxLicenceFile = 1 << 20
	maxTemplate    = 1 << 20
)

// File is a file the extractor can read.
type File struct {
	Path    string
	Size    int64
	Present bool
}

// Source reads a Commit's files.
type Source interface {
	// ReadAll reads a whole small file.
	ReadAll(ctx context.Context, path string, limit int64) ([]byte, error)
	// ReaderAt reads ranges of a file of the given size.
	ReaderAt(ctx context.Context, path string, size int64) io.ReaderAt
}

// DatasetLayout gives a dataset Commit's Configs and their Splits
// (DATA-9). The datasets package implements it.
type DatasetLayout func(ctx context.Context) (configs []string, splits map[string]int, err error)

// errTooLarge marks files over a read limit.
var errTooLarge = errors.New("file is too large to index")

var splitFileRe = regexp.MustCompile(`(?i)-(\d{5})-of-(\d{5})\.gguf$`)

// Extract builds the index of a Commit from its files. It never fails
// because of a malformed file; such files get a per-file error.
func Extract(ctx context.Context, t model.RepoType, files []File, src Source, layout DatasetLayout) (*Summary, error) {
	x := &extractor{ctx: ctx, src: src, files: map[string]File{}}
	s := &Summary{Formats: []string{}}
	for _, f := range files {
		x.files[f.Path] = f
		x.paths = append(x.paths, f.Path)
		s.Size.Total += f.Size
		if f.Present {
			s.Size.Present += f.Size
		}
	}
	sort.Strings(x.paths)
	if err := x.run(s, t, layout); err != nil {
		return nil, err
	}
	return s, nil
}

type extractor struct {
	ctx   context.Context
	src   Source
	files map[string]File
	paths []string

	config    map[string]any
	tokConfig map[string]any
	ggufs     map[string]*gguf.File
	fileInfo  map[string]*FileInfo
}

func (x *extractor) has(p string) bool { _, ok := x.files[p]; return ok }

func (x *extractor) readJSON(p string) (map[string]any, error) {
	f, ok := x.files[p]
	if !ok || !f.Present {
		return nil, nil
	}
	if f.Size > maxJSONFile {
		return nil, fmt.Errorf("%w: %s", errTooLarge, p)
	}
	b, err := x.src.ReadAll(x.ctx, p, maxJSONFile)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s is not a JSON object: %w", p, err)
	}
	return m, nil
}

func (x *extractor) info(p, format string) *FileInfo {
	if fi, ok := x.fileInfo[p]; ok {
		return fi
	}
	fi := &FileInfo{Path: p, Format: format}
	x.fileInfo[p] = fi
	return fi
}

func (x *extractor) run(s *Summary, t model.RepoType, layout DatasetLayout) error {
	x.fileInfo = map[string]*FileInfo{}
	x.ggufs = map[string]*gguf.File{}
	var err error
	if x.config, err = x.readJSON("config.json"); err != nil {
		x.info("config.json", "json").Error = err.Error()
		x.config = nil
	}
	if x.tokConfig, err = x.readJSON("tokenizer_config.json"); err != nil {
		x.info("tokenizer_config.json", "json").Error = err.Error()
		x.tokConfig = nil
	}
	if err := x.readCard(s); err != nil {
		return err
	}
	x.formats(s, t)
	if err := x.safetensors(s); err != nil {
		return err
	}
	if err := x.gguf(s); err != nil {
		return err
	}
	x.architecture(s)
	x.contextLength(s)
	x.chatTemplate(s)
	x.tokens(s)
	x.mlx(s)
	x.licence(s)
	x.taskAndLibrary(s, t)
	x.hubConfig(s)
	if t == model.RepoTypeDataset {
		x.dataset(s, layout)
	}
	if t == model.RepoTypeModel {
		s.Datasets = card.Strings(s.CardData, "datasets")
	}
	x.tags(s, t)
	for _, p := range x.paths {
		if fi, ok := x.fileInfo[p]; ok {
			s.Files = append(s.Files, *fi)
		}
	}
	return x.ctx.Err()
}

func (x *extractor) readCard(s *Summary) error {
	f, ok := x.files["README.md"]
	if !ok || !f.Present {
		return nil
	}
	if f.Size > maxReadme {
		x.info("README.md", "markdown").Error = "the Model card is larger than 4 MiB and was not read"
		return nil
	}
	b, err := x.src.ReadAll(x.ctx, "README.md", maxReadme)
	if err != nil {
		if x.ctx.Err() != nil {
			return x.ctx.Err()
		}
		x.info("README.md", "markdown").Error = err.Error()
		return nil
	}
	if data, err := card.FromReadme(b); err == nil {
		s.CardData = data
	} else {
		x.info("README.md", "markdown").Error = err.Error()
	}
	return nil
}

// Format detection from file extensions and layout (07 "What the index holds").
func (x *extractor) formats(s *Summary, t model.RepoType) {
	set := map[string]bool{}
	stems := map[string]bool{}
	for _, p := range x.paths {
		if strings.HasSuffix(strings.ToLower(p), ".xml") {
			stems[strings.TrimSuffix(p, path.Ext(p))] = true
		}
	}
	for _, p := range x.paths {
		lower := strings.ToLower(p)
		base := path.Base(lower)
		ext := path.Ext(lower)
		switch {
		case ext == ".gguf":
			set["gguf"] = true
		case ext == ".safetensors":
			set["safetensors"] = true
		case ext == ".onnx" || strings.HasPrefix(lower, "onnx/") && ext == ".onnx_data":
			set["onnx"] = true
		case ext == ".bin" && stems[strings.TrimSuffix(p, path.Ext(p))]:
			set["openvino"] = true
		case base == "openvino_model.xml":
			set["openvino"] = true
		case t == model.RepoTypeModel && (ext == ".bin" || ext == ".pt" || ext == ".pth" || ext == ".ckpt"):
			set["pytorch"] = true
		case ext == ".h5" || base == "saved_model.pb" || base == "saved_model.pbtxt":
			set["tensorflow"] = true
		case ext == ".msgpack":
			set["flax"] = true
		case ext == ".mlmodel" || strings.Contains(lower, ".mlpackage/"):
			set["coreml"] = true
		case ext == ".tflite":
			set["tflite"] = true
		}
	}
	if t == model.RepoTypeModel && set["safetensors"] && x.isMLX(s) {
		set["mlx"] = true
	}
	for f := range set {
		s.Formats = append(s.Formats, f)
	}
	sort.Strings(s.Formats)
}

// isMLX applies the rule in 07: safetensors plus config.json with a
// quantization or quantization_config block, and an mlx tag or
// library_name: mlx in the card.
func (x *extractor) isMLX(s *Summary) bool {
	if x.config == nil {
		return false
	}
	_, q1 := x.config["quantization"].(map[string]any)
	_, q2 := x.config["quantization_config"].(map[string]any)
	if !q1 && !q2 {
		return false
	}
	if strings.EqualFold(card.String(s.CardData, "library_name"), "mlx") {
		return true
	}
	return slices.ContainsFunc(card.Strings(s.CardData, "tags"), func(t string) bool { return strings.EqualFold(t, "mlx") })
}

func (x *extractor) safetensors(s *Summary) error {
	byDType := map[string]int64{}
	var total int64
	var files []string
	for _, p := range x.paths {
		if strings.EqualFold(path.Ext(p), ".safetensors") {
			files = append(files, p)
		}
	}
	// Count weights at the root when there are any, so diffusers
	// components and variants in sub-folders do not add up twice.
	var root []string
	for _, p := range files {
		if !strings.Contains(p, "/") {
			root = append(root, p)
		}
	}
	counted := files
	if len(root) > 0 {
		counted = root
	}
	for _, p := range files {
		f := x.files[p]
		fi := x.info(p, "safetensors")
		if !f.Present {
			continue
		}
		h, err := safetensors.Read(x.src.ReaderAt(x.ctx, p, f.Size), f.Size)
		if err != nil {
			if x.ctx.Err() != nil {
				return x.ctx.Err()
			}
			fi.Error = err.Error()
			continue
		}
		by, n := h.Parameters()
		fi.TensorCount = len(h.Tensors)
		fi.Parameters = n
		fi.DType = h.DominantDType()
		if slices.Contains(counted, p) {
			for d, c := range by {
				byDType[d] += c
			}
			total += n
		}
	}
	if total > 0 || len(byDType) > 0 {
		s.Parameters = &Parameters{Total: total, ByDType: byDType}
	}
	return nil
}

func (x *extractor) gguf(s *Summary) error {
	sizes := map[string]int64{}
	for _, p := range x.paths {
		if !strings.EqualFold(path.Ext(p), ".gguf") {
			continue
		}
		f := x.files[p]
		sizes[p] = f.Size
		fi := x.info(p, "gguf")
		if !f.Present {
			continue
		}
		g, err := gguf.Read(x.src.ReaderAt(x.ctx, p, f.Size), f.Size)
		if err != nil {
			if x.ctx.Err() != nil {
				return x.ctx.Err()
			}
			fi.Error = err.Error()
			continue
		}
		x.ggufs[p] = g
		fi.TensorCount = len(g.Tensors)
		fi.Parameters = g.Parameters()
		fi.DType = dominant(g.ParametersByType())
		_, name := g.FileType()
		if q := QuantName(p); q != "" {
			fi.Quant = q
		} else {
			fi.Quant = name
		}
	}
	if len(sizes) == 0 {
		return nil
	}
	groups := GroupQuants(sizes)
	for _, g := range groups {
		q := Quantisation{Name: g.Name, Files: g.Files}
		for _, p := range g.Files {
			if gf, ok := x.ggufs[p]; ok {
				if n, _ := gf.FileType(); n >= 0 {
					q.FileType = &n
					break
				}
			}
		}
		s.Quantisations = append(s.Quantisations, q)
	}
	best, ok := PreferredQuant(groups)
	if !ok {
		return nil
	}
	var first *gguf.File
	var params int64
	for _, p := range best.Files {
		g, ok := x.ggufs[p]
		if !ok {
			continue
		}
		if first == nil {
			first = g
		}
		params += g.Parameters()
	}
	if first == nil {
		return nil
	}
	s.GGUF = &GGUF{Total: params, Architecture: first.Architecture(), ContextLength: first.ContextLength(),
		ChatTemplate: first.ChatTemplate(), BOSToken: first.BOSToken(), EOSToken: first.EOSToken()}
	if s.Parameters == nil {
		s.Parameters = &Parameters{Total: params}
	}
	return nil
}

func dominant(by map[string]int64) string {
	best, bestN := "", int64(-1)
	for d, n := range by {
		if n > bestN || (n == bestN && d < best) {
			best, bestN = d, n
		}
	}
	return best
}

// latestOrder is Ollama's `latest` preference (08-local-tools.md).
var latestOrder = []string{"Q4_K_M", "Q4_K_S", "Q5_K_M", "Q4_0", "Q5_K_S", "Q6_K", "Q8_0", "IQ4_XS", "Q3_K_M", "BF16", "F16"}

// PreferredQuant picks the quant group Ollama's `latest` tag means: Q4_K_M
// if present, else the first of latestOrder present, else the smallest.
func PreferredQuant(groups []QuantGroup) (QuantGroup, bool) {
	if len(groups) == 0 {
		return QuantGroup{}, false
	}
	for _, want := range latestOrder {
		for _, g := range groups {
			if strings.EqualFold(g.Name, want) {
				return g, true
			}
		}
	}
	best := groups[0]
	for _, g := range groups[1:] {
		if g.Size < best.Size || g.Size == best.Size && g.Name < best.Name {
			best = g
		}
	}
	return best, true
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func num(m map[string]any, key string) (int64, bool) {
	switch v := m[key].(type) {
	case float64:
		if v > 0 && v < 1<<53 {
			return int64(v), true
		}
	case string:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

func (x *extractor) architecture(s *Summary) {
	if x.config != nil {
		if a, ok := x.config["architectures"].([]any); ok && len(a) > 0 {
			s.Architecture, _ = a[0].(string)
		}
		s.ModelType = str(x.config, "model_type")
	}
	if s.Architecture == "" && s.GGUF != nil {
		s.Architecture = s.GGUF.Architecture
	}
	if s.ModelType == "" && s.GGUF != nil {
		s.ModelType = s.GGUF.Architecture
	}
}

func (x *extractor) contextLength(s *Summary) {
	if s.GGUF != nil && s.GGUF.ContextLength > 0 {
		s.ContextLength = s.GGUF.ContextLength
		return
	}
	for _, cfg := range []map[string]any{x.config, sub(x.config, "text_config")} {
		if cfg == nil {
			continue
		}
		for _, k := range []string{"max_position_embeddings", "n_positions", "max_seq_len", "seq_length"} {
			if n, ok := num(cfg, k); ok {
				s.ContextLength = n
				return
			}
		}
	}
}

func sub(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	v, _ := m[key].(map[string]any)
	return v
}

func (x *extractor) readText(p string, limit int64) string {
	f, ok := x.files[p]
	if !ok || !f.Present || f.Size > limit {
		return ""
	}
	b, err := x.src.ReadAll(x.ctx, p, limit)
	if err != nil {
		return ""
	}
	return string(b)
}

func (x *extractor) chatTemplate(s *Summary) {
	if s.GGUF != nil && s.GGUF.ChatTemplate != "" {
		s.ChatTemplate = s.GGUF.ChatTemplate
		return
	}
	if x.tokConfig != nil {
		switch v := x.tokConfig["chat_template"].(type) {
		case string:
			s.ChatTemplate = v
			return
		case []any:
			var first string
			for _, item := range v {
				m, _ := item.(map[string]any)
				if first == "" {
					first = str(m, "template")
				}
				if str(m, "name") == "default" {
					s.ChatTemplate = str(m, "template")
					return
				}
			}
			if first != "" {
				s.ChatTemplate = first
				return
			}
		}
	}
	if t := x.readText("chat_template.jinja", maxTemplate); t != "" {
		s.ChatTemplate = t
		return
	}
	if ct, err := x.readJSON("chat_template.json"); err == nil && ct != nil {
		s.ChatTemplate = str(ct, "chat_template")
	}
}

func tokenText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		return str(t, "content")
	}
	return ""
}

func (x *extractor) tokens(s *Summary) {
	tok := Tokens{}
	if s.GGUF != nil {
		tok = Tokens{BOS: s.GGUF.BOSToken, EOS: s.GGUF.EOSToken}
	}
	if x.tokConfig != nil {
		if tok.BOS == "" {
			tok.BOS = tokenText(x.tokConfig["bos_token"])
		}
		if tok.EOS == "" {
			tok.EOS = tokenText(x.tokConfig["eos_token"])
		}
	}
	if tok.BOS == "" && tok.EOS == "" {
		if m, err := x.readJSON("special_tokens_map.json"); err == nil && m != nil {
			tok.BOS, tok.EOS = tokenText(m["bos_token"]), tokenText(m["eos_token"])
		}
	}
	if tok.BOS != "" || tok.EOS != "" {
		s.Tokens = &tok
	}
}

func (x *extractor) mlx(s *Summary) {
	if !slices.Contains(s.Formats, "mlx") {
		return
	}
	q, ok := x.config["quantization"].(map[string]any)
	if !ok {
		q, _ = x.config["quantization_config"].(map[string]any)
	}
	bits, _ := num(q, "bits")
	group, _ := num(q, "group_size")
	name := "mlx"
	if bits > 0 {
		name = fmt.Sprintf("%dbit", bits)
	}
	s.Quantisations = append(s.Quantisations, Quantisation{Name: name, Bits: int(bits), Group: int(group)})
}

var licenceFileRe = regexp.MustCompile(`(?i)^(licen[cs]e|copying)([._-].*)?$`)

func (x *extractor) licence(s *Summary) {
	if id := card.String(s.CardData, "license"); id != "" {
		s.Licence = Licence{ID: strings.ToLower(id), Name: card.String(s.CardData, "license_name"), Link: card.String(s.CardData, "license_link")}
		return
	}
	for _, p := range x.paths {
		if strings.Contains(p, "/") || !licenceFileRe.MatchString(p) {
			continue
		}
		if id := MatchLicence(x.readText(p, maxLicenceFile)); id != "" {
			s.Licence = Licence{ID: id}
			return
		}
	}
}

// pipelineBySuffix maps transformers architecture class suffixes to
// pipeline tags. Longer suffixes come first.
var pipelineBySuffix = []struct{ suffix, task string }{
	{"ForConditionalGenerationWithProjection", "image-text-to-text"},
	{"ForSpeechSeq2Seq", "automatic-speech-recognition"},
	{"ForCTC", "automatic-speech-recognition"},
	{"ForCausalLM", "text-generation"},
	{"LMHeadModel", "text-generation"},
	{"ForMaskedLM", "fill-mask"},
	{"ForSeq2SeqLM", "text2text-generation"},
	{"ForSequenceClassification", "text-classification"},
	{"ForTokenClassification", "token-classification"},
	{"ForQuestionAnswering", "question-answering"},
	{"ForMultipleChoice", "multiple-choice"},
	{"ForImageClassification", "image-classification"},
	{"ForObjectDetection", "object-detection"},
	{"ForSemanticSegmentation", "image-segmentation"},
	{"ForInstanceSegmentation", "image-segmentation"},
	{"ForDepthEstimation", "depth-estimation"},
	{"ForAudioClassification", "audio-classification"},
	{"ForImageTextToText", "image-text-to-text"},
	{"ForVision2Seq", "image-text-to-text"},
	{"ForTextToSpeech", "text-to-speech"},
	{"ForTextToWaveform", "text-to-audio"},
}

// visionPrefixes mark conditional-generation models that take images.
var visionPrefixes = []string{"Llava", "Idefics", "Paligemma", "Qwen2VL", "Qwen2_5_VL", "Mllama", "Gemma3", "Blip", "Kosmos", "Pix2Struct", "Florence"}

func (x *extractor) taskAndLibrary(s *Summary, t model.RepoType) {
	s.PipelineTag = card.String(s.CardData, "pipeline_tag")
	s.LibraryName = card.String(s.CardData, "library_name")
	if t != model.RepoTypeModel {
		return
	}
	sentence := x.has("modules.json") || x.has("config_sentence_transformers.json")
	diffusers := x.has("model_index.json")
	if s.LibraryName == "" {
		switch {
		case sentence:
			s.LibraryName = "sentence-transformers"
		case diffusers:
			s.LibraryName = "diffusers"
		case x.has("adapter_config.json"):
			s.LibraryName = "peft"
		case slices.Contains(s.Formats, "mlx"):
			s.LibraryName = "mlx"
		case x.config != nil && (x.config["architectures"] != nil || x.config["model_type"] != nil):
			s.LibraryName = "transformers"
		case len(s.Formats) == 1 && s.Formats[0] == "gguf":
			s.LibraryName = "gguf"
		}
	}
	if s.PipelineTag != "" {
		return
	}
	switch {
	case sentence:
		s.PipelineTag = "sentence-similarity"
		return
	case diffusers:
		if idx, err := x.readJSON("model_index.json"); err == nil {
			cls := str(idx, "_class_name")
			switch {
			case strings.Contains(cls, "Img2Img"):
				s.PipelineTag = "image-to-image"
			case strings.Contains(cls, "Video"):
				s.PipelineTag = "text-to-video"
			case strings.HasSuffix(cls, "Pipeline"):
				s.PipelineTag = "text-to-image"
			}
		}
		return
	}
	arch := s.Architecture
	if x.config == nil && s.GGUF != nil && s.GGUF.Architecture != "" {
		if strings.Contains(s.GGUF.Architecture, "clip") || strings.Contains(s.GGUF.Architecture, "bert") {
			return
		}
		s.PipelineTag = "text-generation"
		return
	}
	if strings.HasSuffix(arch, "ForConditionalGeneration") {
		for _, p := range visionPrefixes {
			if strings.HasPrefix(arch, p) {
				s.PipelineTag = "image-text-to-text"
				return
			}
		}
		if strings.HasPrefix(arch, "Whisper") {
			s.PipelineTag = "automatic-speech-recognition"
			return
		}
		s.PipelineTag = "text2text-generation"
		return
	}
	for _, m := range pipelineBySuffix {
		if strings.HasSuffix(arch, m.suffix) {
			s.PipelineTag = m.task
			return
		}
	}
	if arch != "" && strings.HasSuffix(arch, "Model") {
		s.PipelineTag = "feature-extraction"
	}
}

// hubConfig is the Hub's `config` field: a small part of config.json and
// tokenizer_config.json.
func (x *extractor) hubConfig(s *Summary) {
	out := map[string]any{}
	if x.config != nil {
		for _, k := range []string{"architectures", "model_type", "auto_map", "quantization_config", "task_specific_params"} {
			if v, ok := x.config[k]; ok {
				out[k] = v
			}
		}
	} else if s.GGUF != nil && s.GGUF.Architecture != "" {
		out["model_type"] = s.GGUF.Architecture
	}
	if x.tokConfig != nil {
		tc := map[string]any{}
		for _, k := range []string{"bos_token", "eos_token", "pad_token", "unk_token", "sep_token", "cls_token", "mask_token"} {
			if v, ok := x.tokConfig[k]; ok && v != nil {
				tc[k] = v
			}
		}
		if s.ChatTemplate != "" {
			tc["chat_template"] = s.ChatTemplate
		}
		if len(tc) > 0 {
			out["tokenizer_config"] = tc
		}
	}
	if len(out) == 0 {
		return
	}
	b, err := json.Marshal(out)
	if err == nil {
		s.Config = b
	}
}

var datasetFormats = map[string]string{
	".parquet": "parquet", ".csv": "csv", ".tsv": "csv", ".json": "json", ".jsonl": "json", ".ndjson": "json",
	".txt": "text", ".arrow": "arrow", ".tar": "webdataset",
}

var mediaModality = map[string]string{
	".png": "image", ".jpg": "image", ".jpeg": "image", ".webp": "image", ".gif": "image", ".bmp": "image", ".tif": "image", ".tiff": "image",
	".wav": "audio", ".mp3": "audio", ".flac": "audio", ".ogg": "audio", ".opus": "audio", ".m4a": "audio",
	".mp4": "video", ".webm": "video", ".mov": "video", ".avi": "video", ".mkv": "video",
	".pdf": "document",
}

func (x *extractor) dataset(s *Summary, layout DatasetLayout) {
	d := &DatasetSummary{}
	formats := map[string]bool{}
	modalities := map[string]bool{}
	for _, p := range x.paths {
		lower := strings.ToLower(p)
		if strings.HasPrefix(path.Base(lower), ".") || lower == "readme.md" {
			continue
		}
		ext := path.Ext(strings.TrimSuffix(strings.TrimSuffix(lower, ".gz"), ".zst"))
		if f, ok := datasetFormats[ext]; ok {
			formats[f] = true
			switch f {
			case "parquet", "csv", "arrow":
				modalities["tabular"] = true
			case "text":
				modalities["text"] = true
			}
		}
		if m, ok := mediaModality[ext]; ok {
			modalities[m] = true
			switch m {
			case "image":
				formats["imagefolder"] = true
			case "audio":
				formats["audiofolder"] = true
			}
		}
	}
	if formats["json"] || formats["csv"] || formats["parquet"] {
		modalities["text"] = true
	}
	for f := range formats {
		d.Formats = append(d.Formats, f)
	}
	for m := range modalities {
		d.Modalities = append(d.Modalities, m)
	}
	sort.Strings(d.Formats)
	sort.Strings(d.Modalities)
	d.Tasks = card.Strings(s.CardData, "task_categories")
	d.Languages = card.Strings(s.CardData, "language")
	d.SizeCategory = card.String(s.CardData, "size_categories")
	d.PrettyName = card.String(s.CardData, "pretty_name")
	if layout != nil {
		if configs, splits, err := layout(x.ctx); err == nil {
			d.Configs, d.Splits = configs, splits
		}
	}
	s.Dataset = d
}

func (x *extractor) tags(s *Summary, t model.RepoType) {
	seen := map[string]bool{}
	add := func(tag string) {
		if tag != "" && !seen[tag] {
			seen[tag] = true
			s.Tags = append(s.Tags, tag)
		}
	}
	for _, tag := range card.Strings(s.CardData, "tags") {
		add(tag)
	}
	if t == model.RepoTypeModel {
		add(s.LibraryName)
		for _, f := range s.Formats {
			add(f)
		}
		add(s.ModelType)
		add(s.PipelineTag)
		for _, bm := range card.Strings(s.CardData, "base_model") {
			add("base_model:" + bm)
		}
		for _, ds := range card.Strings(s.CardData, "datasets") {
			add("dataset:" + ds)
		}
		for _, l := range card.Strings(s.CardData, "language") {
			add(l)
		}
	}
	if d := s.Dataset; d != nil {
		for _, tc := range d.Tasks {
			add("task_categories:" + tc)
		}
		for _, l := range d.Languages {
			add("language:" + l)
		}
		if d.SizeCategory != "" {
			add("size_categories:" + d.SizeCategory)
		}
		for _, f := range d.Formats {
			add("format:" + f)
		}
		for _, m := range d.Modalities {
			add("modality:" + m)
		}
	}
	if s.Licence.ID != "" {
		add("license:" + s.Licence.ID)
	}
}

// IsSplitShard reports whether a GGUF path is shard n of m, and returns n.
func IsSplitShard(p string) (n int, ok bool) {
	m := splitFileRe.FindStringSubmatch(p)
	if m == nil {
		return 0, false
	}
	n, _ = strconv.Atoi(m[1])
	return n, true
}
