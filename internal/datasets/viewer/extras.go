package viewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/croissant"
	"github.com/jamesagarside/hangar/internal/datasets/layout"
	"github.com/jamesagarside/hangar/internal/datasets/meta"
	"github.com/jamesagarside/hangar/internal/datasets/pipeline"
	"github.com/jamesagarside/hangar/internal/model"
)

// optInOut answers /opt-in-out-urls with the Hub's empty result: Hangar
// does not scan for opted-out image URLs.
func (s *Service) optInOut(ctx context.Context, p model.Principal, id model.RepoID, config, split string) (any, *state, error) {
	st, err := s.head(ctx, p, id, auth.ActionRead)
	if err != nil {
		return nil, nil, err
	}
	if config != "" && !st.hasConfig(config) {
		return nil, st, fmt.Errorf("%w: config %s", datasets.ErrNotFound, config)
	}
	return object{{Name: "urls_columns", Value: []string{}}, {Name: "has_urls_columns", Value: false}, {Name: "num_opt_in_urls", Value: 0},
		{Name: "num_opt_out_urls", Value: 0}, {Name: "num_scanned_rows", Value: 0}, {Name: "num_urls", Value: 0}, {Name: "full_scan", Value: false}}, st, nil
}

// presidio answers /presidio-entities with the Hub's empty result.
func (s *Service) presidio(ctx context.Context, p model.Principal, id model.RepoID, config, split string) (any, *state, error) {
	st, err := s.head(ctx, p, id, auth.ActionRead)
	if err != nil {
		return nil, nil, err
	}
	if config != "" && !st.hasConfig(config) {
		return nil, st, fmt.Errorf("%w: config %s", datasets.ErrNotFound, config)
	}
	return object{{Name: "scanned_columns", Value: []string{}}, {Name: "num_rows_with_person_entities", Value: 0},
		{Name: "num_rows_with_phone_number_entities", Value: 0}, {Name: "num_rows_with_email_address_entities", Value: 0},
		{Name: "num_rows_with_sensitive_pii", Value: 0}, {Name: "num_scanned_rows", Value: 0}, {Name: "has_scanned_columns", Value: false},
		{Name: "full_scan", Value: true}}, st, nil
}

// Croissant implements datasets.Viewer.
func (s *Service) Croissant(ctx context.Context, p model.Principal, id model.RepoID) (json.RawMessage, error) {
	st, err := s.head(ctx, p, id, auth.ActionRead)
	if err != nil {
		return nil, err
	}
	if st.failed != "" {
		return nil, fmt.Errorf("%w: %s", ErrFailed, st.failed)
	}
	c, err := layout.ReadCard(ctx, s.d.Repos, st.repo.ID, st.commit)
	if err != nil {
		return nil, fmt.Errorf("%w: the dataset card is not valid: %w", ErrFailed, err)
	}
	base := s.o.PublicURL
	in := croissant.Input{Dataset: st.dataset(), URL: base + "/" + st.repo.ID.Path(),
		ExportURL: base + "/" + st.repo.ID.Path() + "/tree/refs%2Fconvert%2Fparquet", PrettyName: c.PrettyName,
		Description: meta.PlainText(c.Body), Licenses: c.Licenses, Citation: c.Citation,
		Creator: st.repo.ID.Namespace, CreatorURL: base + "/" + st.repo.ID.Namespace}
	var infos []*pipeline.ConfigInfo
	for _, name := range st.configs() {
		info, err := s.info(ctx, st, name)
		if errors.Is(err, pipeline.ErrNoInfo) {
			continue
		}
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
		cfg := croissant.Config{Name: name, Features: info.Features, Partial: info.Partial}
		for _, sp := range info.Splits {
			cfg.Splits = append(cfg.Splits, sp.Name)
		}
		in.Configs = append(in.Configs, cfg)
	}
	if len(in.Configs) == 0 {
		return nil, datasets.ErrNotReady
	}
	summary := meta.Summarise(c, infos)
	in.Keywords = summary.Keywords()
	return croissant.Build(in)
}
