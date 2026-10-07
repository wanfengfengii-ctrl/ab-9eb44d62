package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// WireType is one of the four binary wire types a field may use.
type WireType string

const (
	WireVarint          WireType = "VARINT"
	WireFixed32         WireType = "FIXED32"
	WireFixed64         WireType = "FIXED64"
	WireLengthDelimited WireType = "LENGTH_DELIMITED"
)

// ParseWireType normalizes a user-supplied wire type (case-insensitive, "-"
// and "_" interchangeable) and reports whether it is one of the four
// supported wire types.
func ParseWireType(s string) (WireType, bool) {
	norm := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "-", "_"))
	switch wt := WireType(norm); wt {
	case WireVarint, WireFixed32, WireFixed64, WireLengthDelimited:
		return wt, true
	}
	return "", false
}

// MaxFieldNumber is the largest assignable field number (2^29-1, as in
// Protocol Buffers).
const MaxFieldNumber = 536870911

var (
	fieldNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	subjectRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// Field binds a name to a field number and wire type.
type Field struct {
	Name     string   `json:"name"`
	Number   int      `json:"number"`
	WireType WireType `json:"wireType"`
}

// SubmitRequest is the body of POST /api/schemas/{subject}/versions.
type SubmitRequest struct {
	RequestID       string  `json:"requestId"`
	Version         int     `json:"version"`
	Fields          []Field `json:"fields"`
	ReservedNumbers []int   `json:"reservedNumbers"`
}

// Stable machine-readable error codes returned in every error body.
const (
	CodeValidation        = "VALIDATION_ERROR"
	CodeDuplicateNumber   = "DUPLICATE_FIELD_NUMBER"
	CodeDuplicateName     = "DUPLICATE_FIELD_NAME"
	CodeDuplicateReserved = "DUPLICATE_RESERVED_NUMBER"
	CodeVersionNotNext    = "VERSION_NOT_NEXT"
	CodeRequestConflict   = "REQUEST_CONFLICT"
	CodeNumberNotRetained = "NUMBER_NOT_RETAINED"
	CodeReservedRevoked   = "RESERVED_NUMBER_REVOKED"
	CodeReservedReused    = "RESERVED_NUMBER_REUSED"
	CodeFieldRenamed      = "FIELD_RENAMED"
	CodeWireTypeChanged   = "WIRE_TYPE_CHANGED"
	CodeSubjectNotFound   = "SUBJECT_NOT_FOUND"
	CodeInternal          = "INTERNAL_ERROR"
)

// APIError is the stable machine-readable error payload. FirstViolation is
// the lowest field number involved in the violation, when applicable.
type APIError struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	CurrentVersion int    `json:"currentVersion"`
	FirstViolation *int   `json:"firstViolation,omitempty"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

type errorBody struct {
	Error *APIError `json:"error"`
}

// validateShape checks a decoded request for structural sanity and
// normalizes wire types in place. It does not look at any stored state.
func validateShape(req *SubmitRequest) *APIError {
	if req.RequestID == "" || len(req.RequestID) > 128 {
		return &APIError{Code: CodeValidation, Message: "requestId is required and must be at most 128 characters"}
	}
	if req.Version < 1 {
		return &APIError{Code: CodeValidation, Message: "version must be a positive integer"}
	}
	seenNumber := map[int]bool{}
	seenName := map[string]bool{}
	dupNumber, dupName := -1, -1
	for i := range req.Fields {
		f := &req.Fields[i]
		if !fieldNameRe.MatchString(f.Name) {
			e := &APIError{Code: CodeValidation, Message: fmt.Sprintf("invalid field name %q", f.Name)}
			if f.Number >= 1 && f.Number <= MaxFieldNumber {
				n := f.Number
				e.FirstViolation = &n
			}
			return e
		}
		if f.Number < 1 || f.Number > MaxFieldNumber {
			return &APIError{Code: CodeValidation, Message: fmt.Sprintf("field %q has out-of-range number %d (must be 1..%d)", f.Name, f.Number, MaxFieldNumber)}
		}
		wt, ok := ParseWireType(string(f.WireType))
		if !ok {
			n := f.Number
			return &APIError{Code: CodeValidation, Message: fmt.Sprintf("field %q has unknown wire type %q (want VARINT, FIXED32, FIXED64 or LENGTH_DELIMITED)", f.Name, f.WireType), FirstViolation: &n}
		}
		f.WireType = wt
		if seenNumber[f.Number] {
			dupNumber = minViolation(dupNumber, f.Number)
		}
		seenNumber[f.Number] = true
		if seenName[f.Name] {
			dupName = minViolation(dupName, f.Number)
		}
		seenName[f.Name] = true
	}
	if dupNumber >= 0 {
		return &APIError{Code: CodeDuplicateNumber, Message: fmt.Sprintf("field number %d is assigned more than once", dupNumber), FirstViolation: &dupNumber}
	}
	if dupName >= 0 {
		return &APIError{Code: CodeDuplicateName, Message: "a field name is assigned more than once", FirstViolation: &dupName}
	}
	seenReserved := map[int]bool{}
	dupReserved := -1
	for _, n := range req.ReservedNumbers {
		if n < 1 || n > MaxFieldNumber {
			return &APIError{Code: CodeValidation, Message: fmt.Sprintf("reserved number %d is out of range (must be 1..%d)", n, MaxFieldNumber)}
		}
		if seenReserved[n] {
			dupReserved = minViolation(dupReserved, n)
		}
		seenReserved[n] = true
	}
	if dupReserved >= 0 {
		return &APIError{Code: CodeDuplicateReserved, Message: fmt.Sprintf("reserved number %d is listed more than once", dupReserved), FirstViolation: &dupReserved}
	}
	return nil
}

// validateEvolution checks a shape-valid request against the current state of
// its subject (cur == nil means the subject does not exist yet). Rules are
// evaluated in a fixed order and the first failing rule wins; the reported
// FirstViolation is the lowest offending field number for that rule.
func validateEvolution(cur *subjectState, req *SubmitRequest) *APIError {
	newReserved := map[int]bool{}
	for _, n := range req.ReservedNumbers {
		newReserved[n] = true
	}
	newByNumber := map[int]Field{}
	for _, f := range req.Fields {
		newByNumber[f.Number] = f
	}

	var curFields []Field
	var curReserved map[int]bool
	if cur != nil && len(cur.Versions) > 0 {
		curFields = cur.Versions[len(cur.Versions)-1].Fields
		curReserved = cur.Reserved
	}

	// 1. A number dropped from the field list must be retained as reserved.
	first := -1
	for _, cf := range curFields {
		if _, kept := newByNumber[cf.Number]; kept {
			continue
		}
		if newReserved[cf.Number] {
			continue
		}
		first = minViolation(first, cf.Number)
	}
	if first >= 0 {
		return &APIError{Code: CodeNumberNotRetained, Message: fmt.Sprintf("field number %d was removed without retaining its number in reservedNumbers", first), FirstViolation: &first}
	}

	// 2. Historical reserved numbers must never be revoked.
	first = -1
	for n := range curReserved {
		if !newReserved[n] {
			first = minViolation(first, n)
		}
	}
	if first >= 0 {
		return &APIError{Code: CodeReservedRevoked, Message: fmt.Sprintf("reserved number %d from an earlier version is missing in reservedNumbers", first), FirstViolation: &first}
	}

	// 3. Reserved numbers must never be reused as fields.
	first = -1
	for _, f := range req.Fields {
		if curReserved[f.Number] || newReserved[f.Number] {
			first = minViolation(first, f.Number)
		}
	}
	if first >= 0 {
		return &APIError{Code: CodeReservedReused, Message: fmt.Sprintf("field number %d reuses a reserved (tombstoned) number", first), FirstViolation: &first}
	}

	// 4. An existing number must keep its name.
	first = -1
	for _, cf := range curFields {
		if nf, ok := newByNumber[cf.Number]; ok && nf.Name != cf.Name {
			first = minViolation(first, cf.Number)
		}
	}
	if first >= 0 {
		return &APIError{Code: CodeFieldRenamed, Message: fmt.Sprintf("field number %d was renamed (was %q)", first, curFields[0].Name), FirstViolation: &first}
	}

	// 5. An existing number must keep its wire type.
	first = -1
	for _, cf := range curFields {
		if nf, ok := newByNumber[cf.Number]; ok && nf.WireType != cf.WireType {
			first = minViolation(first, cf.Number)
		}
	}
	if first >= 0 {
		return &APIError{Code: CodeWireTypeChanged, Message: fmt.Sprintf("field number %d changed wire type", first), FirstViolation: &first}
	}

	return nil
}

// minViolation folds n into the running minimum; -1 means "none yet".
func minViolation(cur, n int) int {
	if cur < 0 || n < cur {
		return n
	}
	return cur
}

func sortedFields(in []Field) []Field {
	out := append([]Field{}, in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

func sortedInts(in []int) []int {
	out := append([]int{}, in...)
	sort.Ints(out)
	return out
}

// fingerprint is the canonical content identity of a request, used to decide
// whether a repeated requestId carries the same content. Field and reserved
// number ordering are normalized so semantically identical payloads match.
func fingerprint(req *SubmitRequest) string {
	b, _ := json.Marshal(struct {
		Version  int     `json:"v"`
		Fields   []Field `json:"f"`
		Reserved []int   `json:"r"`
	}{req.Version, sortedFields(req.Fields), sortedInts(req.ReservedNumbers)})
	return string(b)
}
