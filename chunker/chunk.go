/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package chunker

import (
	"bufio"
	"bytes"
	"compress/gzip"
	encjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/hypermodeinc/dgraph/v24/enc"
	"github.com/hypermodeinc/dgraph/v24/lex"
	"github.com/hypermodeinc/dgraph/v24/x"
)

// Chunker describes the interface to parse and process the input to the live and bulk loaders.
type Chunker interface {
	Chunk(r *bufio.Reader) (*bytes.Buffer, error)
	Parse(chunkBuf *bytes.Buffer) error
	NQuads() *NQuadBuffer
}

type rdfChunker struct {
	lexer *lex.Lexer
	nqs   *NQuadBuffer
}

func (rc *rdfChunker) NQuads() *NQuadBuffer {
	return rc.nqs
}

type jsonChunker struct {
	nqs    *NQuadBuffer
	inList bool
}

func (jc *jsonChunker) NQuads() *NQuadBuffer {
	return jc.nqs
}

// InputFormat represents the multiple formats supported by Chunker.
type InputFormat byte

const (
	// UnknownFormat is a constant to denote a format not supported by the bulk/live loaders.
	UnknownFormat InputFormat = iota
	// RdfFormat is a constant to denote the input to the live/bulk loader is in the RDF format.
	RdfFormat
	// JsonFormat is a constant to denote the input to the live/bulk loader is in the JSON format.
	JsonFormat
)

// NewChunker returns a new chunker for the specified format.
func NewChunker(inputFormat InputFormat, batchSize int) Chunker {
	switch inputFormat {
	case RdfFormat:
		return &rdfChunker{
			nqs:   NewNQuadBuffer(batchSize),
			lexer: &lex.Lexer{},
		}
	case JsonFormat:
		return &jsonChunker{
			nqs: NewNQuadBuffer(batchSize),
		}
	default:
		x.Panic(errors.New("unknown input format"))
		return nil
	}
}

// Chunk reads the input line by line until one of the following 3 conditions happens
// 1) the EOF is reached
// 2) 1e5 lines have been read
// 3) some unexpected error happened
func (*rdfChunker) Chunk(r *bufio.Reader) (*bytes.Buffer, error) {
	batch := new(bytes.Buffer)
	batch.Grow(1 << 20)
	for range 100000 {
		slc, err := r.ReadSlice('\n')
		if err == io.EOF {
			if _, err := batch.Write(slc); err != nil {
				return nil, err
			}
			return batch, err
		}
		if err == bufio.ErrBufferFull {
			// This should only happen infrequently.
			if _, err := batch.Write(slc); err != nil {
				return nil, err
			}
			var str string
			str, err = r.ReadString('\n')
			if err == io.EOF {
				if _, err := batch.WriteString(str); err != nil {
					return nil, err
				}
				return batch, err
			}
			if err != nil {
				return nil, err
			}
			if _, err := batch.WriteString(str); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, err := batch.Write(slc); err != nil {
			return nil, err
		}
	}
	return batch, nil
}

// Parse is not thread-safe. Only call it serially, because it reuses lexer object.
func (rc *rdfChunker) Parse(chunkBuf *bytes.Buffer) error {
	if chunkBuf == nil || chunkBuf.Len() == 0 {
		return nil
	}

	for chunkBuf.Len() > 0 {
		str, err := chunkBuf.ReadString('\n')
		if err != nil && err != io.EOF {
			x.Check(err)
		}

		nq, err := ParseRDF(str, rc.lexer)
		switch {
		case err == ErrEmpty:
			continue // blank line or comment
		case err != nil:
			return fmt.Errorf("while parsing line %q: %w", str, err)
		default:
			rc.nqs.Push(&nq)
		}
	}
	return nil
}

// Chunk tries to consume multiple top-level maps from the reader until a size threshold is
// reached, or the end of file is reached.
func (jc *jsonChunker) Chunk(r *bufio.Reader) (*bytes.Buffer, error) {
	ch, err := jc.nextRune(r)
	if err != nil {
		return nil, err
	}
	// If the file starts with a list rune [, we set the inList flag, and keep consuming maps
	// until we reach the threshold.
	switch {
	case ch == '[':
		jc.inList = true
	case ch == '{':
		// put the rune back for it to be consumed in the consumeMap function
		if err := r.UnreadRune(); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("file is not JSON")
	}

	out := new(bytes.Buffer)
	if _, err := out.WriteRune('['); err != nil {
		return nil, err
	}
	hasMapsBefore := false
	for out.Len() < 1e5 {
		if hasMapsBefore {
			if _, err := out.WriteRune(','); err != nil {
				return nil, err
			}
		}
		if err := jc.consumeMap(r, out); err != nil {
			return nil, err
		}
		hasMapsBefore = true

		// handle the legal termination cases, by checking the next rune after the map
		ch, err := jc.nextRune(r)
		if err == io.EOF {
			// handles the EOF case, return the buffer which represents the top level map
			if jc.inList {
				return nil, errors.New("JSON file ends abruptly, expecting ]")
			}

			if _, err := out.WriteRune(']'); err != nil {
				return nil, err
			}
			return out, io.EOF
		} else if err != nil {
			return nil, err
		}

		if ch == ']' {
			if !jc.inList {
				return nil, errors.New("JSON map is followed by an extraneous ]")
			}

			// validate that there are no more non-space chars after the ]
			if slurpSpace(r) != io.EOF {
				return nil, errors.New("not all of JSON file consumed")
			}

			if _, err := out.WriteRune(']'); err != nil {
				return nil, err
			}
			return out, io.EOF
		}

		// In the non-termination cases, ensure at least one map has been consumed, and
		// the only allowed char after the map is ",".
		if out.Len() == 1 { // 1 represents the [ inserted before the for loop
			return nil, fmt.Errorf("illegal rune found \"%c\", expecting {", ch)
		}
		if ch != ',' {
			return nil, fmt.Errorf("JSON map is followed by illegal rune \"%c\"", ch)
		}
	}
	if _, err := out.WriteRune(']'); err != nil {
		return nil, err
	}
	return out, nil
}

// consumeMap consumes the next map from the reader, and stores the result into the buffer out.
// After ignoring spaces, if the reader does not begin with {, no rune will be consumed
// from the reader.
func (jc *jsonChunker) consumeMap(r *bufio.Reader, out *bytes.Buffer) error {
	// Just find the matching closing brace. Let the JSON-to-nquad parser in the mapper worry
	// about whether everything in between is valid JSON or not.
	depth := 0
	for {
		ch, err := jc.nextRune(r)
		if err != nil {
			return errors.New("Malformed JSON")
		}
		if depth == 0 && ch != '{' {
			// We encountered a beginning rune that's not {,
			// unread the char and return without consuming anything.
			if err := r.UnreadRune(); err != nil {
				return err
			}
			return nil
		}

		if _, err := out.WriteRune(ch); err != nil {
			return err
		}
		switch ch {
		case '{':
			depth++
		case '}':
			depth--
		case '"':
			if err := slurpQuoted(r, out); err != nil {
				return err
			}
		default:
			// We just write the rune to out, and let the Go JSON parser do its job.
		}
		if depth <= 0 {
			break
		}
	}
	return nil
}

// nextRune ignores any number of spaces that may precede a rune
func (*jsonChunker) nextRune(r *bufio.Reader) (rune, error) {
	if err := slurpSpace(r); err != nil {
		return ' ', err
	}
	ch, _, err := r.ReadRune()
	if err != nil {
		return ' ', err
	}
	return ch, nil
}

func (jc *jsonChunker) Parse(chunkBuf *bytes.Buffer) error {
	if chunkBuf == nil || chunkBuf.Len() == 0 {
		return nil
	}

	return jc.nqs.ParseJSON(chunkBuf.Bytes(), SetNquads)
}

func slurpSpace(r *bufio.Reader) error {
	for {
		ch, _, err := r.ReadRune()
		if err != nil {
			return err
		}
		if !unicode.IsSpace(ch) {
			x.Check(r.UnreadRune())
			return nil
		}
	}
}

func slurpQuoted(r *bufio.Reader, out *bytes.Buffer) error {
	for {
		ch, _, err := r.ReadRune()
		if err != nil {
			return err
		}
		if _, err := out.WriteRune(ch); err != nil {
			return err
		}

		if ch == '\\' {
			// Pick one more rune.
			esc, _, err := r.ReadRune()
			if err != nil {
				return err
			}
			if _, err := out.WriteRune(esc); err != nil {
				return err
			}
			continue
		}
		if ch == '"' {
			return nil
		}
	}
}

// FileReader returns an open reader on the given file. Gzip-compressed input is detected
// and decompressed automatically even without the gz extension. The key, if non-nil,
// is used to decrypt the file. The caller is responsible for calling the returned cleanup
// function when done with the reader.
func FileReader(file string, key x.Sensitive) (*bufio.Reader, func()) {
	var f *os.File
	var err error
	if file == "-" {
		f = os.Stdin
	} else {
		f, err = os.Open(file)
	}

	x.Check(err)

	return StreamReader(file, key, f)
}

// StreamReader returns a bufio given a ReadCloser. The file is passed just to check for .gz files
func StreamReader(file string, key x.Sensitive, f io.ReadCloser) (
	rd *bufio.Reader, cleanup func()) {
	cleanup = func() { _ = f.Close() }

	if filepath.Ext(file) == ".gz" {
		r, err := enc.GetReader(key, f)
		x.Check(err)
		gzr, err := gzip.NewReader(r)
		x.Check(err)
		rd = bufio.NewReader(gzr)
		cleanup = func() { _ = f.Close(); _ = gzr.Close() }
	} else {
		rd = bufio.NewReader(f)
		buf, _ := rd.Peek(512)

		typ := http.DetectContentType(buf)
		if typ == "application/x-gzip" {
			gzr, err := gzip.NewReader(rd)
			x.Check(err)
			rd = bufio.NewReader(gzr)
			cleanup = func() { _ = f.Close(); _ = gzr.Close() }
		}
	}

	return rd, cleanup
}

// IsJSONData returns true if the reader, which should be at the start of the stream, is reading
// a JSON stream, false otherwise.
func IsJSONData(r *bufio.Reader) (bool, error) {
	buf, err := r.Peek(512)
	if err != nil && err != io.EOF {
		return false, err
	}

	de := encjson.NewDecoder(bytes.NewReader(buf))
	_, err = de.Token()

	return err == nil, nil
}

// DataFormat returns a file's data format (RDF, JSON, or unknown) based on the filename
// or the user-provided format option. The file extension has precedence.
func DataFormat(filename string, format string) InputFormat {
	format = strings.ToLower(format)
	filename = strings.TrimSuffix(strings.ToLower(filename), ".gz")
	switch {
	case strings.HasSuffix(filename, ".rdf") || format == "rdf":
		return RdfFormat
	case strings.HasSuffix(filename, ".json") || format == "json":
		return JsonFormat
	default:
		return UnknownFormat
	}
}
