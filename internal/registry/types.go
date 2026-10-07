// Package registry implements the core domain logic of the telemetry
// schema registry: linearly versioned field schemas per subject,
// cumulative tombstoning of deleted field numbers, idempotent
// publishing and durable persistence.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WireType is one of the four supported binary wire types.
type WireType string

const (
	WireVarint          WireType = "VARINT"
	WireFixed32         WireType = "FIXED32"
	WireFixed64         WireType = "FIXED64"
	WireLengthDelimited WireType = "LENGTH_DELIMITED"
)

// WireTypes lists every supported wire type.
func WireTypes() []WireType {
	return []WireType{WireVarint, WireFixed32, WireFixed64, WireLengthDelimited}
}

// Valid reports whether w is one of the four supported wire types.
func (w WireType) Valid() bool {
	switch w {
	case WireVarint, WireFixed32, WireFixed64, WireLengthDelimited:
		return true
	default:
		return false
	}
}

// Field binds a field name to a field number and a wire type.
type Field struct {
	Name     string   `json:"name"`
	Number   int      `json:"number"`
	WireType WireType `json:"wireType"`
}

// Version is one accepted schema version of a subject.
type Version struct {
	Version   int       `json:"version"`
	Fields    []Field   `json:"fields"`
	RequestID string    `json:"requestId"`
	CreatedAt time.Time `json:"createdAt"`
}

// idemRecord stores the outcome of a previously seen requestId so that
// retries of the same request can be replayed verbatim.
type idemRecord struct {
	ContentHash string          `json:"contentHash"`
	Status      int             `json:"status"`
	Body        json.RawMessage `json:"body"`
}

// subject is the durable per-subject state.
type subject struct {
	CurrentVersion int                   `json:"currentVersion"`
	Versions       []Version             `json:"versions"`
	Reserved       []int                 `json:"reservedNumbers"`
	Idempotency    map[string]idemRecord `json:"idempotency,omitempty"`
}

func (s *subject) reservedSet() map[int]bool {
	set := make(map[int]bool, len(s.Reserved))
	for _, n := range s.Reserved {
		set[n] = true
	}
	return set
}

func (s *subject) currentFields() []Field {
	if len(s.Versions) == 0 {
		return nil
	}
	return s.Versions[len(s.Versions)-1].Fields
}

func cloneSubject(s *subject) *subject {
	out := &subject{
		CurrentVersion: s.CurrentVersion,
		Versions:       append([]Version(nil), s.Versions...),
		Reserved:       append([]int(nil), s.Reserved...),
	}
	if s.Idempotency != nil {
		out.Idempotency = make(map[string]idemRecord, len(s.Idempotency))
		for k, v := range s.Idempotency {
			out.Idempotency[k] = v
		}
	}
	return out
}

// PublishRequest is the payload of POST /api/schemas/{subject}/versions.
type PublishRequest struct {
	RequestID string   `json:"requestId"`
	Version   *int     `json:"version"`
	Fields    *[]Field `json:"fields"`
}

// PublishResponse is returned when a version is accepted.
type PublishResponse struct {
	Subject         string    `json:"subject"`
	Version         int       `json:"version"`
	RequestID       string    `json:"requestId"`
	Fields          []Field   `json:"fields"`
	ReservedNumbers []int     `json:"reservedNumbers"`
	CreatedAt       time.Time `json:"createdAt"`
}

// SchemaResponse is returned by GET /api/schemas/{subject}.
type SchemaResponse struct {
	Subject         string  `json:"subject"`
	CurrentVersion  int     `json:"currentVersion"`
	Fields          []Field `json:"fields"`
	ReservedNumbers []int   `json:"reservedNumbers"`
	Versions        []int   `json:"versions"`
}

// contentHash produces a canonical fingerprint of the request content that
// an idempotency record is bound to. Field ordering is normalized so that
// semantically identical payloads hash equally.
func contentHash(req PublishRequest) string {
	version := ""
	if req.Version != nil {
		version = strconv.Itoa(*req.Version)
	}
	var fields []Field
	if req.Fields != nil {
		fields = append([]Field(nil), (*req.Fields)...)
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Number < fields[j].Number })
	var b strings.Builder
	b.WriteString(version)
	for _, f := range fields {
		fmt.Fprintf(&b, "\x00%s\x00%d\x00%s", f.Name, f.Number, f.WireType)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func sortedFields(in []Field) []Field {
	out := append([]Field{}, in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

func sortedInts(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func intPtr(v int) *int { return &v }
