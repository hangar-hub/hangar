-- What Repo listings filter, sort and count facets on (02-architecture.md
-- "Scale targets": 100,000 Repos). The Repo service keeps one row per Repo,
-- built from the index results at its default branch head and its licence
-- setting, so a listing answers in SQL instead of reading every Repo's
-- index results. version is the version of how the row was built.
CREATE TABLE repo_facts (
    repo_id TEXT PRIMARY KEY REFERENCES repos (id) ON DELETE CASCADE,
    version integer NOT NULL,
    parameters bigint NOT NULL DEFAULT 0,
    context_length bigint NOT NULL DEFAULT 0,
    num_rows bigint NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE INDEX repo_facts_version_idx ON repo_facts (version);
--bun:split

-- The facet values of each Repo: format, task, library, quant, licence,
-- modality, language and size_category. folded is the value with its case
-- folded, for filters, which ignore case.
CREATE TABLE repo_facet_values (
    repo_id TEXT NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    facet text NOT NULL,
    value text NOT NULL,
    folded text NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (repo_id, facet, value)
);
--bun:split
CREATE INDEX repo_facet_values_folded_idx ON repo_facet_values (facet, folded, repo_id);
--bun:split

-- Repos whose repo_facts rows are out of date. The triggers below add a row
-- whenever what a row is built from changes, whoever changes it: a Repo is
-- created, its settings or default branch change, a ref moves, index
-- results are stored or a dataset's Split sizes change. A listing rebuilds
-- those Repos' rows and deletes the rows it read. There is no foreign key,
-- so a Repo's purge can add rows while its refs are deleted.
CREATE TABLE repo_facts_changes (
    id INTEGER PRIMARY KEY,
    repo_id TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
--bun:split
CREATE TRIGGER repo_facts_repos_insert AFTER INSERT ON repos BEGIN
    INSERT INTO repo_facts_changes (repo_id) VALUES (NEW.id);
END;
--bun:split
CREATE TRIGGER repo_facts_repos_update AFTER UPDATE OF settings, default_branch ON repos BEGIN
    INSERT INTO repo_facts_changes (repo_id) VALUES (NEW.id);
END;
--bun:split
CREATE TRIGGER repo_facts_refs_insert AFTER INSERT ON refs BEGIN
    INSERT INTO repo_facts_changes (repo_id) VALUES (NEW.repo_id);
END;
--bun:split
CREATE TRIGGER repo_facts_refs_update AFTER UPDATE ON refs BEGIN
    INSERT INTO repo_facts_changes (repo_id) VALUES (NEW.repo_id);
END;
--bun:split
CREATE TRIGGER repo_facts_refs_delete AFTER DELETE ON refs BEGIN
    INSERT INTO repo_facts_changes (repo_id) VALUES (OLD.repo_id);
END;
--bun:split
CREATE TRIGGER repo_facts_metadata_insert AFTER INSERT ON metadata BEGIN
    INSERT INTO repo_facts_changes (repo_id) VALUES (NEW.repo_id);
END;
--bun:split
CREATE TRIGGER repo_facts_metadata_update AFTER UPDATE ON metadata BEGIN
    INSERT INTO repo_facts_changes (repo_id) VALUES (NEW.repo_id);
END;
--bun:split
CREATE TRIGGER repo_facts_dataset_splits_insert AFTER INSERT ON dataset_splits BEGIN
    INSERT INTO repo_facts_changes (repo_id) VALUES (NEW.repo_id);
END;
--bun:split
CREATE TRIGGER repo_facts_dataset_splits_update AFTER UPDATE ON dataset_splits BEGIN
    INSERT INTO repo_facts_changes (repo_id) VALUES (NEW.repo_id);
END;
--bun:split
CREATE TRIGGER repo_facts_dataset_splits_delete AFTER DELETE ON dataset_splits BEGIN
    INSERT INTO repo_facts_changes (repo_id) VALUES (OLD.repo_id);
END;
--bun:split

-- Every Repo that exists now gets its row on the first listing.
INSERT INTO repo_facts_changes (repo_id) SELECT id FROM repos;
