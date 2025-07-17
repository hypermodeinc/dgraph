/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package tok

import (
	"github.com/pkg/errors"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

var (
	enLangTag, _ = language.Parse("en")
)

// GetTokenizerForLang returns the correct full-text tokenizer for the given language.
func GetTokenizerForLang(t Tokenizer, lang string) Tokenizer {
	if lang == "" {
		return t
	}
	switch t.(type) {
	case FullTextTokenizer:
		// We must return a new instance because another goroutine might be calling this
		// with a different lang.
		return FullTextTokenizer{lang: lang}
	case TermTokenizer:
		return TermTokenizer{lang: lang}
	case ExactTokenizer:
		langTag, err := language.Parse(lang)
		// We default to english if the language is not supported.
		if err != nil {
			langTag = enLangTag
		}
		// If this gets expensive memory-vise, then convert it to sync.Pool.
		return ExactTokenizer{langBase: LangBase(lang), cl: collate.New(langTag),
			buffer: &collate.Buffer{}}
	default:
		return t
	}
}

// GetTokens returns the tokens for the given tokenizer ID and value.
// funcArgs should only have one element which is the value that needs to be tokenized.
func GetTokens(id byte, funcArgs ...string) ([]string, error) {
	if l := len(funcArgs); l != 1 {
		return nil, errors.Errorf("Function requires 1 arguments, but got %d", l)
	}
	tokenizer, ok := GetTokenizerByID(id)
	if !ok {
		return nil, errors.Errorf("No tokenizer was found with id %v", id)
	}
	return BuildTokens(funcArgs[0], tokenizer)
}

// GetTermTokens returns the term tokens for the given value.
func GetTermTokens(funcArgs []string) ([]string, error) {
	return GetTokens(IdentTerm, funcArgs...)
}

func GetShinglesTokens(funcArgs []string, lang string) ([]string, error) {
	return BuildTokens(funcArgs[0], ShinglesTokenizer{lang: lang})
}

// GetFullTextTokens returns the full-text tokens for the given value.
func GetFullTextTokens(funcArgs []string, lang string) ([]string, error) {
	if l := len(funcArgs); l != 1 {
		return nil, errors.Errorf("Function requires 1 arguments, but got %d", l)
	}
	return BuildTokens(funcArgs[0], FullTextTokenizer{lang: lang})
}
