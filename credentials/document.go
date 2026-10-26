package credentials

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var errInvalidJSON = errors.New("credentials: document is not valid JSON")

// ParseDocument validates a credentials document: a single JSON object with
// no duplicate keys (JSON parsers disagree on which duplicate wins, which is
// a trap for secrets), at most MaxPlaintextBytes. Errors name a line and
// column, never content.
func ParseDocument(data []byte) error {
	_, err := parseDocument(data)
	return err
}

func parseDocument(data []byte) (map[string]any, error) {
	if len(data) > MaxPlaintextBytes {
		return nil, fmt.Errorf("credentials: document exceeds %d bytes", MaxPlaintextBytes)
	}
	if err := checkStructure(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil || doc == nil {
		return nil, locate(data, err)
	}
	return doc, nil
}

// checkStructure walks the token stream: the top level must be one object,
// object keys must be unique, and nothing may follow the object.
func checkStructure(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	first, err := dec.Token()
	if err != nil {
		return locate(data, err)
	}
	if delim, ok := first.(json.Delim); !ok || delim != '{' {
		return errors.New("credentials: the document must be a JSON object")
	}
	if err := walkObject(dec, data); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("credentials: unexpected data after the JSON object")
		}
		return locate(data, err)
	}
	return nil
}

// walkObject consumes members up to and including the closing brace.
func walkObject(dec *json.Decoder, data []byte) error {
	seen := map[string]bool{}
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return locate(data, err)
		}
		key, _ := keyToken.(string) // the decoder reports a key that is not a string as a syntax error
		if seen[key] {
			return fmt.Errorf("credentials: duplicate key at %s", position(data, dec.InputOffset()))
		}
		seen[key] = true
		if err := walkValue(dec, data); err != nil {
			return err
		}
	}
	_, err := dec.Token() // closing brace
	return locate(data, err)
}

func walkValue(dec *json.Decoder, data []byte) error {
	token, err := dec.Token()
	if err != nil {
		return locate(data, err)
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim == '{' {
		return walkObject(dec, data)
	}
	// An array: the decoder reports a stray closing delimiter where a value
	// is expected as a syntax error, so no other delimiter reaches here.
	for dec.More() {
		if err := walkValue(dec, data); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return locate(data, err)
}

func locate(data []byte, err error) error {
	if err == nil {
		return nil
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Errorf("%w: syntax error at %s", errInvalidJSON, position(data, syntax.Offset))
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: unexpected end of input", errInvalidJSON)
	}
	return errInvalidJSON
}

func position(data []byte, offset int64) string {
	line, col := 1, 1
	for _, b := range data[:min(max(offset, 0), int64(len(data)))] {
		if b == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return fmt.Sprintf("line %d, column %d", line, col)
}
