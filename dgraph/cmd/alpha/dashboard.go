/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package alpha

import (
	"encoding/json"
	"net/http"

	"github.com/hypermodeinc/dgraph/v25/x"
)

type keyword struct {
	// Type could be a predicate, function etc.
	Type string `json:"type"`
	Name string `json:"name"`
}

type keywords struct {
	Keywords []keyword `json:"keywords"`
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}
	x.Check2(w.Write([]byte(
		"Dgraph browser is available for running separately using the dgraph-ratel binary")))
}

// Used to return a list of keywords, so that UI can show them for autocompletion.
func keywordHandler(w http.ResponseWriter, r *http.Request) {
	x.AddCorsHeaders(w)
	if r.Method == http.MethodOptions {
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, x.ErrorInvalidMethod, http.StatusBadRequest)
		return
	}

	var kws keywords
	predefined := []string{
		"@cascade",
		"@facets",
		"@filter",
		"@if",
		"@normalize",
		"after",
		"allofterms",
		"shingles",
		"alloftext",
		"and",
		"anyofterms",
		"anyoftext",
		"as",
		"avg",
		"ceil",
		"cond",
		"contains",
		"count",
		"delete",
		"eq",
		"exact",
		"exp",
		"expand",
		"first",
		"floor",
		"fulltext",
		"func",
		"ge",
		"gt",
		"index",
		"intersects",
		"le",
		"len",
		"ln",
		"logbase",
		"lt",
		"math",
		"max",
		"min",
		"mutation",
		"near",
		"not",
		"offset",
		"or",
		"orderasc",
		"orderdesc",
		"pow",
		"recurse",
		"regexp",
		"reverse",
		"schema",
		"since",
		"set",
		"sqrt",
		"sum",
		"term",
		"tokenizer",
		"type",
		"uid",
		"within",
		"upsert",
	}

	for _, w := range predefined {
		kws.Keywords = append(kws.Keywords, keyword{
			Name: w,
		})
	}
	js, err := json.Marshal(kws)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		x.Check2(w.Write([]byte(err.Error())))
		return
	}
	x.Check2(w.Write(js))
}
