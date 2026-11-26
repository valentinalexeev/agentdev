// Package json is a local build stub (not committed): encoding/json behind the segmentio API the MCP SDK uses.
package json

import (
	std "encoding/json"
	"io"
)

type Decoder struct{ *std.Decoder }

func NewDecoder(r io.Reader) *Decoder               { return &Decoder{std.NewDecoder(r)} }
func (d *Decoder) DontMatchCaseInsensitiveStructFields() {}

type Encoder = std.Encoder

func NewEncoder(w io.Writer) *Encoder { return std.NewEncoder(w) }
