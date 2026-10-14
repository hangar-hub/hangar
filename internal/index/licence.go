package index

import (
	"strings"
)

// licenceRule matches a licence text by phrases that only it contains.
// IDs are the lower-case identifiers the Hub uses in `license:` metadata,
// which are SPDX identifiers where one exists.
type licenceRule struct {
	id   string
	all  []string
	none []string
}

// licenceRules are checked in order; more specific rules come first.
var licenceRules = []licenceRule{
	{id: "llama4", all: []string{"llama 4 community license"}},
	{id: "llama3.3", all: []string{"llama 3.3 community license"}},
	{id: "llama3.2", all: []string{"llama 3.2 community license"}},
	{id: "llama3.1", all: []string{"llama 3.1 community license"}},
	{id: "llama3", all: []string{"meta llama 3 community license"}},
	{id: "llama2", all: []string{"llama 2 community license"}},
	{id: "gemma", all: []string{"gemma terms of use"}},
	{id: "creativeml-openrail-m", all: []string{"creativeml open rail-m"}},
	{id: "bigscience-openrail-m", all: []string{"bigscience open rail-m"}},
	{id: "agpl-3.0", all: []string{"gnu affero general public license", "version 3"}},
	{id: "lgpl-3.0", all: []string{"gnu lesser general public license", "version 3"}},
	{id: "lgpl-2.1", all: []string{"gnu lesser general public license", "version 2.1"}},
	{id: "gpl-3.0", all: []string{"gnu general public license", "version 3"}, none: []string{"lesser general public", "affero"}},
	{id: "gpl-2.0", all: []string{"gnu general public license", "version 2"}, none: []string{"lesser general public", "library general public", "affero"}},
	{id: "mpl-2.0", all: []string{"mozilla public license", "version 2.0"}},
	{id: "apache-2.0", all: []string{"apache license", "version 2.0"}},
	{id: "cc-by-nc-sa-4.0", all: []string{"attribution-noncommercial-sharealike 4.0 international"}},
	{id: "cc-by-nc-nd-4.0", all: []string{"attribution-noncommercial-noderivatives 4.0 international"}},
	{id: "cc-by-nc-4.0", all: []string{"attribution-noncommercial 4.0 international"}},
	{id: "cc-by-sa-4.0", all: []string{"attribution-sharealike 4.0 international"}},
	{id: "cc-by-nd-4.0", all: []string{"attribution-noderivatives 4.0 international"}},
	{id: "cc-by-4.0", all: []string{"creative commons attribution 4.0 international"}},
	{id: "cc0-1.0", all: []string{"cc0 1.0 universal"}},
	{id: "unlicense", all: []string{"this is free and unencumbered software released into the public domain"}},
	{id: "bsl-1.0", all: []string{"boost software license - version 1.0"}},
	{id: "isc", all: []string{"permission to use, copy, modify, and/or distribute this software for any purpose with or without fee is hereby granted"}},
	{id: "mit", all: []string{"permission is hereby granted, free of charge, to any person obtaining a copy", "the software is provided \"as is\""}},
	{id: "bsd-3-clause", all: []string{"redistribution and use in source and binary forms", "neither the name of"}},
	{id: "bsd-2-clause", all: []string{"redistribution and use in source and binary forms"}},
	{id: "openrail", all: []string{"open rail"}},
}

// MatchLicence returns the licence ID of a licence text, or "" when no
// rule matches. Matching ignores case and runs of whitespace.
func MatchLicence(text string) string {
	norm := strings.Join(strings.Fields(strings.ToLower(text)), " ")
	norm = strings.NewReplacer("“", `"`, "”", `"`).Replace(norm)
	for _, r := range licenceRules {
		ok := true
		for _, p := range r.all {
			if !strings.Contains(norm, p) {
				ok = false
				break
			}
		}
		for _, p := range r.none {
			if strings.Contains(norm, p) {
				ok = false
				break
			}
		}
		if ok {
			return r.id
		}
	}
	return ""
}
