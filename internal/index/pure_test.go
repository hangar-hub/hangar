package index_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/index"
)

func TestPreferredQuant(t *testing.T) {
	_, ok := index.PreferredQuant(nil)
	require.False(t, ok)
	g, _ := index.PreferredQuant([]index.QuantGroup{{Name: "Q8_0", Size: 8}, {Name: "Q4_K_M", Size: 4}})
	require.Equal(t, "Q4_K_M", g.Name)
	g, _ = index.PreferredQuant([]index.QuantGroup{{Name: "Q8_0", Size: 8}, {Name: "Q5_K_M", Size: 5}})
	require.Equal(t, "Q5_K_M", g.Name)
	g, _ = index.PreferredQuant([]index.QuantGroup{{Name: "custom-b", Size: 8}, {Name: "custom-a", Size: 3}, {Name: "IQ1_S", Size: 3}})
	require.Equal(t, "IQ1_S", g.Name, "otherwise the smallest, then by name")
	n, ok := index.IsSplitShard("x/model-00002-of-00003.gguf")
	require.True(t, ok)
	require.Equal(t, 2, n)
	_, ok = index.IsSplitShard("model.gguf")
	require.False(t, ok)
}

func TestMatchLicence(t *testing.T) {
	cases := map[string]string{
		"Apache License\nVersion 2.0, January 2004":                                       "apache-2.0",
		"GNU GENERAL PUBLIC LICENSE\nVersion 3, 29 June 2007":                             "gpl-3.0",
		"GNU GENERAL PUBLIC LICENSE\nVersion 2, June 1991":                                "gpl-2.0",
		"GNU LESSER GENERAL PUBLIC LICENSE Version 3":                                     "lgpl-3.0",
		"GNU AFFERO GENERAL PUBLIC LICENSE Version 3":                                     "agpl-3.0",
		"Redistribution and use in source and binary forms ... Neither the name of":       "bsd-3-clause",
		"Redistribution and use in source and binary forms, with or without modification": "bsd-2-clause",
		"LLAMA 3.1 COMMUNITY LICENSE AGREEMENT":                                           "llama3.1",
		"META LLAMA 3 COMMUNITY LICENSE AGREEMENT":                                        "llama3",
		"Gemma Terms of Use": "gemma",
		"Creative Commons Attribution 4.0 International Public License": "cc-by-4.0",
		"Attribution-NonCommercial 4.0 International":                   "cc-by-nc-4.0",
		"CC0 1.0 Universal":                  "cc0-1.0",
		"Mozilla Public License Version 2.0": "mpl-2.0",
		"This is free and unencumbered software released into the public domain.": "unlicense",
		"Some bespoke terms": "",
	}
	for text, want := range cases {
		require.Equal(t, want, index.MatchLicence(text), text)
	}
}
