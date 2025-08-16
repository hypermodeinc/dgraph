/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package tok

import (
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type encL struct {
	ints   []int64
	tokens []string
}

type byEnc struct{ encL }

func (o byEnc) Less(i, j int) bool { return o.ints[i] < o.ints[j] }

func (o byEnc) Len() int { return len(o.ints) }

func (o byEnc) Swap(i, j int) {
	o.ints[i], o.ints[j] = o.ints[j], o.ints[i]
	o.tokens[i], o.tokens[j] = o.tokens[j], o.tokens[i]
}

func BenchmarkNGramTokenizer(b *testing.B) {
	tokenizer := NGramTokenizer{}
	for i := 0; i < b.N; i++ {
		_, err := BuildTokens("economy series like brother evening tough guess attorney student ago article own identify where care allow pay access decade period during pass fail term real report identify security manage try past account care off rock subject chance seek over effect address article full most believe feel court six involve miss country try if threat care same available tell such process language agreement access poor strong reach cold section past way my seem order live consumer home imagine think return couple office majority answer discover figure college girl Republican enter form recently year experience economic read show necessary general lot bag buy along their painting player information certainly ground soon learn without write determine sure choice sell love subject decide ball death leave happy worry know early main address condition west room he plan weight step loss actually ready provide far fine best nice fund great your scene woman service check feel song ahead material hot leg contain knowledge article lawyer cold", tokenizer)
		require.NoError(b, err)
	}
}

func TestIntEncoding(t *testing.T) {
	a := int64(1<<24 + 10)
	b := int64(-1<<24 - 1)
	c := int64(math.MaxInt64)
	d := int64(math.MinInt64)
	enc := encL{}
	arr := []int64{a, b, c, d, 1, 2, 3, 4, -1, -2, -3, 0, 234, 10000, 123, -1543}
	enc.ints = arr
	for _, it := range arr {
		encoded := encodeInt(it)
		enc.tokens = append(enc.tokens, encoded)
	}
	sort.Sort(byEnc{enc})
	for i := 1; i < len(enc.tokens); i++ {
		// The corresponding string tokens should be greater.
		require.True(t, enc.tokens[i-1] < enc.tokens[i], "%d %v vs %d %v",
			enc.ints[i-1], []byte(enc.tokens[i-1]), enc.ints[i], []byte(enc.tokens[i]))
	}
}

func TestFullTextTokenizer(t *testing.T) {
	tokenizer, has := GetTokenizer("fulltext")
	require.True(t, has)
	require.NotNil(t, tokenizer)

	tokens, err := BuildTokens("Stemming works!", GetTokenizerForLang(tokenizer, "en"))
	require.NoError(t, err)
	require.Equal(t, 2, len(tokens))
	id := tokenizer.Identifier()
	require.Equal(t, []string{encodeToken("stem", id), encodeToken("work", id)}, tokens)
}

func TestHourTokenizer(t *testing.T) {
	var err error
	tokenizer, has := GetTokenizer("hour")
	require.True(t, has)
	require.NotNil(t, tokenizer)
	dt, err := time.Parse(time.RFC3339, "2017-01-01T12:12:12Z")
	require.NoError(t, err)

	tokens, err := BuildTokens(dt, tokenizer)
	require.NoError(t, err)
	require.Equal(t, 1, len(tokens))
	require.Equal(t, 1+2*4, len(tokens[0]))
}

func TestDayTokenizer(t *testing.T) {
	var err error
	tokenizer, has := GetTokenizer("day")
	require.True(t, has)
	require.NotNil(t, tokenizer)
	dt, err := time.Parse(time.RFC3339, "2017-01-01T12:12:12Z")
	require.NoError(t, err)

	tokens, err := BuildTokens(dt, tokenizer)
	require.NoError(t, err)
	require.Equal(t, 1, len(tokens))
	require.Equal(t, 1+2*3, len(tokens[0]))
}

func TestMonthTokenizer(t *testing.T) {
	var err error
	tokenizer, has := GetTokenizer("month")
	require.True(t, has)
	require.NotNil(t, tokenizer)
	dt, err := time.Parse(time.RFC3339, "2017-01-01T12:12:12Z")
	require.NoError(t, err)

	tokens, err := BuildTokens(dt, tokenizer)
	require.NoError(t, err)
	require.Equal(t, 1, len(tokens))
	require.Equal(t, 1+2*2, len(tokens[0]))
}

func TestDateTimeTokenizer(t *testing.T) {
	var err error
	tokenizer, has := GetTokenizer("year")
	require.True(t, has)
	require.NotNil(t, tokenizer)
	dt, err := time.Parse(time.RFC3339, "2017-01-01T12:12:12Z")
	require.NoError(t, err)

	tokens, err := BuildTokens(dt, tokenizer)
	require.NoError(t, err)
	require.Equal(t, 1, len(tokens))
	require.Equal(t, 1+2, len(tokens[0]))
}

func TestFullTextTokenizerLang(t *testing.T) {
	tokenizer, has := GetTokenizer("fulltext")
	require.True(t, has)
	require.NotNil(t, tokenizer)

	tokens, err := BuildTokens("Katzen und Auffassung und Auffassung", GetTokenizerForLang(tokenizer, "de"))
	require.NoError(t, err)
	require.Equal(t, 2, len(tokens))
	id := tokenizer.Identifier()
	// tokens should be sorted and unique
	require.Equal(t, []string{encodeToken("auffassung", id), encodeToken("katz", id)}, tokens)
}

func TestTermTokenizer(t *testing.T) {
	tokenizer, has := GetTokenizer("term")
	require.True(t, has)
	require.NotNil(t, tokenizer)

	tokens, err := BuildTokens("Tokenizer works works!", tokenizer)
	require.NoError(t, err)
	require.Equal(t, 2, len(tokens))
	id := tokenizer.Identifier()
	require.Equal(t, []string{encodeToken("tokenizer", id), encodeToken("works", id)}, tokens)

	// TEMPORARILY COMMENTED OUT AS THIS IS THE IDEAL BEHAVIOUR. WE ARE NOT THERE YET.
	/*
		tokens, err = BuildTokens("Barack Obama made Obamacare", tokenizer)
		require.NoError(t, err)
		require.Equal(t, 3, len(tokens))
		require.Equal(t, []string{
			encodeToken("barack obama", id),
			encodeToken("made", id),
			encodeToken("obamacare", id),
		})
	*/
}

func TestTrigramTokenizer(t *testing.T) {
	tokenizer, has := GetTokenizer("trigram")
	require.True(t, has)
	require.NotNil(t, tokenizer)
	tokens, err := BuildTokens("Dgraph rocks!", tokenizer)
	require.NoError(t, err)
	require.Equal(t, 11, len(tokens))
	id := tokenizer.Identifier()
	expected := []string{
		encodeToken("Dgr", id),
		encodeToken("gra", id),
		encodeToken("rap", id),
		encodeToken("aph", id),
		encodeToken("ph ", id),
		encodeToken("h r", id),
		encodeToken(" ro", id),
		encodeToken("roc", id),
		encodeToken("ock", id),
		encodeToken("cks", id),
		encodeToken("ks!", id),
	}
	sort.Strings(expected)
	require.Equal(t, expected, tokens)
}

func TestGetFullTextTokens(t *testing.T) {
	val := "Our chief weapon is surprise...surprise and fear...fear and surprise...." +
		"Our two weapons are fear and surprise...and ruthless efficiency.... " +
		"Our three weapons are fear, surprise, and ruthless efficiency..."
	tokens, err := (&FullTextTokenizer{lang: "en"}).Tokens(val)
	require.NoError(t, err)

	expected := []string{"chief", "weapon", "surpris", "fear", "ruthless", "effici", "two", "three"}
	sort.Strings(expected)

	// ensure that tokens are sorted and unique
	require.Equal(t, expected, tokens)
}

func TestGetFullTextTokens1(t *testing.T) {
	tokens, err := GetFullTextTokens([]string{"Quick brown fox"}, "en")
	require.NoError(t, err)
	require.NotNil(t, tokens)
	require.Equal(t, 3, len(tokens))
}

func TestGetFullTextTokensInvalidLang(t *testing.T) {
	tokens, err := GetFullTextTokens([]string{"Quick brown fox"}, "xxx_such_language")
	require.NoError(t, err)
	require.NotNil(t, tokens)
	require.Equal(t, 3, len(tokens))
}

// NOTE: The Chinese/Japanese/Korean tests were are based on assuming that the
// output is correct (and adding it to the test), with some verification using
// Google translate.

func TestFullTextTokenizerCJKChinese(t *testing.T) {
	tokenizer, has := GetTokenizer("fulltext")
	require.True(t, has)
	require.NotNil(t, tokenizer)

	got, err := BuildTokens("他是一个薪水很高的商人", GetTokenizerForLang(tokenizer, "zh"))
	require.NoError(t, err)

	id := tokenizer.Identifier()
	wantToks := []string{
		encodeToken("一个", id),
		encodeToken("个薪", id),
		encodeToken("他是", id),
		encodeToken("商人", id),
		encodeToken("很高", id),
		encodeToken("是一", id),
		encodeToken("水很", id),
		encodeToken("的商", id),
		encodeToken("薪水", id),
		encodeToken("高的", id),
	}
	require.Equal(t, wantToks, got)
	checkSortedAndUnique(t, got)
}

func TestFullTextTokenizerCJKKorean(t *testing.T) {
	tokenizer, has := GetTokenizer("fulltext")
	require.True(t, has)
	require.NotNil(t, tokenizer)

	got, err := BuildTokens("그는 큰 급여를 가진 사업가입니다.", GetTokenizerForLang(tokenizer, "ko"))
	require.NoError(t, err)

	id := tokenizer.Identifier()
	wantToks := []string{
		encodeToken("가진", id),
		encodeToken("그는", id),
		encodeToken("급여를", id),
		encodeToken("사업가입니다", id),
		encodeToken("큰", id),
	}
	require.Equal(t, wantToks, got)
	checkSortedAndUnique(t, got)
}

func TestFullTextTokenizerCJKJapanese(t *testing.T) {
	tokenizer, has := GetTokenizer("fulltext")
	require.True(t, has)
	require.NotNil(t, tokenizer)

	got, err := BuildTokens("彼は大きな給与を持つ実業家です", GetTokenizerForLang(tokenizer, "ja"))
	require.NoError(t, err)

	id := tokenizer.Identifier()
	wantToks := []string{
		encodeToken("きな", id),
		encodeToken("つ実", id),
		encodeToken("です", id),
		encodeToken("な給", id),
		encodeToken("は大", id),
		encodeToken("を持", id),
		encodeToken("与を", id),
		encodeToken("大き", id),
		encodeToken("実業", id),
		encodeToken("家で", id),
		encodeToken("彼は", id),
		encodeToken("持つ", id),
		encodeToken("業家", id),
		encodeToken("給与", id),
	}
	require.Equal(t, wantToks, got)
	checkSortedAndUnique(t, got)
}

func TestTermTokenizeCJKChinese(t *testing.T) {
	tokenizer, ok := GetTokenizer("term")
	require.True(t, ok)
	require.NotNil(t, tokenizer)

	got, err := BuildTokens("第一轮 第二轮 第一轮", GetTokenizerForLang(tokenizer, "zh"))
	require.NoError(t, err)

	id := tokenizer.Identifier()
	wantToks := []string{
		encodeToken("第一轮", id),
		encodeToken("第二轮", id),
	}
	require.Equal(t, wantToks, got)
	checkSortedAndUnique(t, got)

}

func checkSortedAndUnique(t *testing.T, tokens []string) {
	if !sort.StringsAreSorted(tokens) {
		t.Error("tokens were not sorted")
	}
	set := make(map[string]struct{})
	for _, tok := range tokens {
		if _, ok := set[tok]; ok {
			if ok {
				t.Error("tokens are not unique")
			}
		}
		set[tok] = struct{}{}
	}
}

func TestNGramTokenizer(t *testing.T) {
	tokenizer, has := GetTokenizer("ngram")
	require.True(t, has)
	require.NotNil(t, tokenizer)

	tokens, err := BuildTokens("quick brown fox", GetTokenizerForLang(tokenizer, "en"))
	require.NoError(t, err)
	require.Greater(t, len(tokens), 0)

	id := tokenizer.Identifier()

	expectedTokens := []string{
		encodeToken("quick", id),
		encodeToken("brown", id),
		encodeToken("fox", id),
		encodeToken("quick brown", id),
		encodeToken("brown fox", id),
		encodeToken("quick brown fox", id),
	}

	for _, expected := range expectedTokens {
		require.Contains(t, tokens, expected, "Expected token %s not found", expected)
	}

	// ngram tokens are not guaranteed to be sorted, just check uniqueness
	set := make(map[string]struct{})
	for _, token := range tokens {
		if _, exists := set[token]; exists {
			t.Error("tokens are not unique")
		}
		set[token] = struct{}{}
	}
}

func TestNGramTokenizerEmpty(t *testing.T) {
	tokenizer, has := GetTokenizer("ngram")
	require.True(t, has)
	require.NotNil(t, tokenizer)

	tokens, err := BuildTokens("", GetTokenizerForLang(tokenizer, "en"))
	require.NoError(t, err)
	require.Equal(t, 0, len(tokens), "Expected 0 tokens for empty string")

	tokens, err = BuildTokens("   ", GetTokenizerForLang(tokenizer, "en"))
	require.NoError(t, err)
	require.Equal(t, 0, len(tokens), "Expected 0 tokens for whitespace only")
}

func TestNGramTokenizerSingleWord(t *testing.T) {
	tokenizer, has := GetTokenizer("ngram")
	require.True(t, has)
	require.NotNil(t, tokenizer)

	tokens, err := BuildTokens("hello", GetTokenizerForLang(tokenizer, "en"))
	require.NoError(t, err)
	require.Greater(t, len(tokens), 0)

	id := tokenizer.Identifier()
	require.Contains(t, tokens, encodeToken("hello", id), "Expected token not found")
	checkSortedAndUnique(t, tokens)
}

func TestNGramTokenizerStopwords(t *testing.T) {
	tokenizer, has := GetTokenizer("ngram")
	require.True(t, has)
	require.NotNil(t, tokenizer)

	tokens, err := BuildTokens("the quick brown fox", GetTokenizerForLang(tokenizer, "en"))
	require.NoError(t, err)
	require.Greater(t, len(tokens), 0, "Expected non-empty tokens for phrase")

	id := tokenizer.Identifier()

	require.NotContains(t, tokens, encodeToken("the", id), "Expected token not found")
	require.NotContains(t, tokens, encodeToken("the quick", id), "Expected token not found")
	require.Contains(t, tokens, encodeToken("quick", id), "Expected token not found")
	require.Contains(t, tokens, encodeToken("brown", id), "Expected token not found")
	require.Contains(t, tokens, encodeToken("fox", id), "Expected token not found")

	set := make(map[string]struct{})
	for _, token := range tokens {
		if _, exists := set[token]; exists {
			t.Error("tokens are not unique")
		}
		set[token] = struct{}{}
	}
}

func TestNGramTokenizerStemming(t *testing.T) {
	tokenizer, has := GetTokenizer("ngram")
	require.True(t, has)
	require.NotNil(t, tokenizer)

	tokens, err := BuildTokens("running quickly", GetTokenizerForLang(tokenizer, "en"))
	require.NoError(t, err)
	require.Greater(t, len(tokens), 0, "Expected non-empty tokens for phrase")

	id := tokenizer.Identifier()

	require.Contains(t, tokens, encodeToken("run", id), "Expected token not found")         // "running" -> "run"
	require.Contains(t, tokens, encodeToken("quickli", id), "Expected token not found")     // "quickly" -> "quickli"
	require.Contains(t, tokens, encodeToken("run quickli", id), "Expected token not found") // bigram with stemmed words

	set := make(map[string]struct{})
	for _, token := range tokens {
		if _, exists := set[token]; exists {
			t.Error("tokens are not unique")
		}
		set[token] = struct{}{}
	}
}

func TestNGramTokenizerLongText(t *testing.T) {
	tokenizer, has := GetTokenizer("ngram")
	require.True(t, has)
	require.NotNil(t, tokenizer)

	longText := "The quick brown fox jumps over the lazy dog. This is a test sentence with multiple words."
	tokens, err := BuildTokens(longText, GetTokenizerForLang(tokenizer, "en"))
	require.NoError(t, err)
	require.Greater(t, len(tokens), 10, "Expected non-empty tokens for phrase")

	id := tokenizer.Identifier()

	testPhrases := []string{
		"quick",
		"brown",
		"fox",
		"quick brown",
		"brown fox",
		"quick brown fox",
		"sentence multiple words",
		"lazy dog test sentence",
	}

	for _, phrase := range testPhrases {
		expectedTokens, err := BuildTokens(phrase, GetTokenizerForLang(tokenizer, "en"))
		require.NoError(t, err, "Failed to tokenize phrase: %s", phrase)

		// Find the appropriate token (could be unigram, bigram, trigram, etc.)
		var expectedToken string
		if strings.Contains(phrase, " ") {
			// For multi-word phrases, find the longest token (likely the full phrase)
			for _, token := range expectedTokens {
				if len(strings.TrimPrefix(token, string(byte(id)))) > len(strings.TrimPrefix(expectedToken, string(byte(id)))) {
					expectedToken = token
				}
			}
		} else {
			// For single words, just take the first token
			if len(expectedTokens) > 0 {
				expectedToken = expectedTokens[0]
			}
		}

		require.NotEmpty(t, expectedToken, "Should find a token for phrase: %s", phrase)
		require.Contains(t, tokens, expectedToken, "Should contain token for phrase: %s", phrase)
	}

	set := make(map[string]struct{})
	for _, token := range tokens {
		if _, exists := set[token]; exists {
			t.Error("tokens are not unique")
		}
		set[token] = struct{}{}
	}
}

func TestNGramTokenizerQueryTokens(t *testing.T) {
	tokenizer := NGramTokenizer{lang: "en"}

	queryTokens, err := BuildNGramQueryTokens("quick brown fox", tokenizer)
	require.NoError(t, err)
	require.Greater(t, len(queryTokens), 0, "QueryTokens should return tokens for trigram input")

	id := tokenizer.Identifier()
	require.Contains(t, queryTokens, encodeToken("quick brown fox", id))

	queryTokens2, err := BuildNGramQueryTokens("hello", tokenizer)
	require.NoError(t, err)
	require.Greater(t, len(queryTokens2), 0, "QueryTokens should return tokens for single word")
	require.Contains(t, queryTokens2, encodeToken("hello", id))

	queryTokens3, err := BuildNGramQueryTokens("", tokenizer)
	require.NoError(t, err)
	require.Equal(t, 0, len(queryTokens3), "QueryTokens should return empty for empty string")
}

func TestNGramTokenizerQueryTokensVariousInputs(t *testing.T) {
	tokenizer := NGramTokenizer{lang: "en"}

	testCases := []struct {
		input            string
		description      string
		expectedNGrams   int
		expectedContains []string
	}{
		{
			input:            "quick brown fox",
			description:      "3 words should generate 1 trigram",
			expectedNGrams:   1,
			expectedContains: []string{"quick brown fox"},
		},
		{
			input:            "hello world",
			description:      "2 words should generate 1 bigram",
			expectedNGrams:   1,
			expectedContains: []string{"hello world"},
		},
		{
			input:            "single",
			description:      "1 word should generate 1 unigram",
			expectedNGrams:   1,
			expectedContains: []string{"single"},
		},
		{
			input:            "one two three four",
			description:      "4 words should generate 2 trigrams",
			expectedNGrams:   2,
			expectedContains: []string{"one two three", "two three four"},
		},
		{
			input:            "the quick brown fox jumps",
			description:      "5 words should generate 2 trigrams (after stopword removal)",
			expectedNGrams:   2,
			expectedContains: []string{"quick brown fox", "brown fox jump"}, // "the" is a stopword
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			queryTokens, err := BuildNGramQueryTokens(tc.input, tokenizer)
			require.NoError(t, err)

			require.Equal(t, tc.expectedNGrams, len(queryTokens),
				"Expected %d n-grams for '%s', got %d", tc.expectedNGrams, tc.input, len(queryTokens))

			id := tokenizer.Identifier()
			for _, expectedNGram := range tc.expectedContains {
				stemmedTokens, err := tokenizer.Tokens(expectedNGram)
				require.NoError(t, err)
				if len(stemmedTokens) > 0 {
					// Find the appropriate n-gram token from the stemmed results
					found := false
					for _, token := range stemmedTokens {
						encodedToken := encodeToken(token, id)
						for _, queryToken := range queryTokens {
							if queryToken == encodedToken {
								found = true
								break
							}
						}
						if found {
							break
						}
					}
					require.True(t, found, "Expected to find stemmed version of '%s' in query tokens", expectedNGram)
				}
			}
		})
	}
}

func TestNGramTokenizerLanguageSupport(t *testing.T) {
	tokenizer, has := GetTokenizer("ngram")
	require.True(t, has)
	require.NotNil(t, tokenizer)

	// Test with different languages
	languages := []string{"en", "es", "fr", "de"}
	for _, lang := range languages {
		tokens, err := BuildTokens("hello world", GetTokenizerForLang(tokenizer, lang))
		require.NoError(t, err, "Failed for language: %s", lang)
		require.Greater(t, len(tokens), 0, "No tokens generated for language: %s", lang)

		set := make(map[string]struct{})
		for _, token := range tokens {
			if _, exists := set[token]; exists {
				t.Error("tokens are not unique")
			}
			set[token] = struct{}{}
		}
	}
}

func TestNGramTokenizerLongTokenHashing(t *testing.T) {
	tokenizer, has := GetTokenizer("ngram")
	require.True(t, has)
	require.NotNil(t, tokenizer)

	// Create a very long text that will generate tokens > 30 characters
	longWords := "supercalifragilisticexpialidocious antidisestablishmentarianism pneumonoultramicroscopicsilicovolcanoconiosiss"
	tokens, err := BuildTokens(longWords, GetTokenizerForLang(tokenizer, "en"))
	require.NoError(t, err)
	require.Greater(t, len(tokens), 0)

	set := make(map[string]struct{})
	for _, token := range tokens {
		if _, exists := set[token]; exists {
			t.Error("tokens are not unique")
		}
		set[token] = struct{}{}
	}

	id := tokenizer.Identifier()
	for _, token := range tokens {
		require.Equal(t, byte(id), token[0], "Token should start with correct identifier")
	}
}

func TestNGramTokenizerNonStringInput(t *testing.T) {
	tokenizer, has := GetTokenizer("ngram")
	require.True(t, has)
	require.NotNil(t, tokenizer)

	tokens, err := BuildTokens(123, GetTokenizerForLang(tokenizer, "en"))
	require.NoError(t, err)
	require.Equal(t, 0, len(tokens), "Expected empty tokens for non-string input")

	tokens2, err := BuildTokens(nil, GetTokenizerForLang(tokenizer, "en"))
	require.NoError(t, err)
	require.Equal(t, 0, len(tokens2), "Expected empty tokens for nil input")
}

func BenchmarkTermTokenizer(b *testing.B) {
	b.Skip() // tmp
}
