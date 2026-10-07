package main

import "testing"

func TestParseWireType(t *testing.T) {
	valid := map[string]WireType{
		"VARINT":             WireVarint,
		"varint":             WireVarint,
		"fixed32":            WireFixed32,
		"FIXED64":            WireFixed64,
		"length_delimited":   WireLengthDelimited,
		"LENGTH-DELIMITED":   WireLengthDelimited,
		" Length_Delimited ": WireLengthDelimited,
	}
	for in, want := range valid {
		got, ok := ParseWireType(in)
		if !ok || got != want {
			t.Errorf("ParseWireType(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "bytes", "fixed16", "VARINT32"} {
		if _, ok := ParseWireType(in); ok {
			t.Errorf("ParseWireType(%q) unexpectedly accepted", in)
		}
	}
}

func TestValidateShape(t *testing.T) {
	base := func() *SubmitRequest {
		return &SubmitRequest{
			RequestID: "r1",
			Version:   1,
			Fields:    []Field{{Name: "a", Number: 1, WireType: WireVarint}},
		}
	}

	t.Run("valid", func(t *testing.T) {
		if err := validateShape(base()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("wire type normalized", func(t *testing.T) {
		r := base()
		r.Fields[0].WireType = "length-delimited"
		if err := validateShape(r); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r.Fields[0].WireType != WireLengthDelimited {
			t.Fatalf("wire type not normalized: %q", r.Fields[0].WireType)
		}
	})

	cases := []struct {
		name   string
		mutate func(*SubmitRequest)
		code   string
	}{
		{"missing requestId", func(r *SubmitRequest) { r.RequestID = "" }, CodeValidation},
		{"version zero", func(r *SubmitRequest) { r.Version = 0 }, CodeValidation},
		{"bad field name", func(r *SubmitRequest) { r.Fields[0].Name = "1bad" }, CodeValidation},
		{"number out of range", func(r *SubmitRequest) { r.Fields[0].Number = MaxFieldNumber + 1 }, CodeValidation},
		{"unknown wire type", func(r *SubmitRequest) { r.Fields[0].WireType = "bytes" }, CodeValidation},
		{"duplicate number", func(r *SubmitRequest) {
			r.Fields = append(r.Fields, Field{Name: "b", Number: 1, WireType: WireFixed32})
		}, CodeDuplicateNumber},
		{"duplicate name", func(r *SubmitRequest) {
			r.Fields = append(r.Fields, Field{Name: "a", Number: 2, WireType: WireFixed32})
		}, CodeDuplicateName},
		{"duplicate reserved", func(r *SubmitRequest) { r.ReservedNumbers = []int{7, 7} }, CodeDuplicateReserved},
		{"reserved out of range", func(r *SubmitRequest) { r.ReservedNumbers = []int{0} }, CodeValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.mutate(r)
			err := validateShape(r)
			if err == nil {
				t.Fatalf("expected %s, got nil", tc.code)
			}
			if err.Code != tc.code {
				t.Fatalf("expected code %s, got %s (%s)", tc.code, err.Code, err.Message)
			}
		})
	}

	t.Run("duplicate number reports lowest", func(t *testing.T) {
		r := base()
		r.Fields = []Field{
			{Name: "a", Number: 5, WireType: WireVarint},
			{Name: "b", Number: 3, WireType: WireVarint},
			{Name: "c", Number: 5, WireType: WireFixed32},
			{Name: "d", Number: 3, WireType: WireFixed32},
		}
		err := validateShape(r)
		if err == nil || err.Code != CodeDuplicateNumber {
			t.Fatalf("expected DUPLICATE_FIELD_NUMBER, got %v", err)
		}
		if err.FirstViolation == nil || *err.FirstViolation != 3 {
			t.Fatalf("expected firstViolation 3, got %v", err.FirstViolation)
		}
	})
}

// twoVersionSubject builds a subject at version 2: fields 1 (alpha/VARINT)
// and 2 (beta/FIXED64) are active, number 3 was deleted in v2 and reserved.
func twoVersionSubject() *subjectState {
	return &subjectState{
		CurrentVersion: 2,
		Versions: []storedVersion{
			{Version: 1, Fields: []Field{
				{Name: "alpha", Number: 1, WireType: WireVarint},
				{Name: "beta", Number: 2, WireType: WireFixed64},
				{Name: "gamma", Number: 3, WireType: WireLengthDelimited},
			}},
			{Version: 2, Fields: []Field{
				{Name: "alpha", Number: 1, WireType: WireVarint},
				{Name: "beta", Number: 2, WireType: WireFixed64},
			}, ReservedNumbers: []int{3}},
		},
		Reserved: map[int]bool{3: true},
	}
}

func TestValidateEvolution(t *testing.T) {
	keep := func() []Field {
		return []Field{
			{Name: "alpha", Number: 1, WireType: WireVarint},
			{Name: "beta", Number: 2, WireType: WireFixed64},
		}
	}

	cases := []struct {
		name      string
		fields    []Field
		reserved  []int
		wantCode  string
		wantFirst int // 0 means expect success
	}{
		{
			name:     "ok: keep all and add field",
			fields:   append(keep(), Field{Name: "delta", Number: 4, WireType: WireFixed32}),
			reserved: []int{3},
		},
		{
			name:     "ok: delete field and retain number",
			fields:   keep()[:1],
			reserved: []int{2, 3},
		},
		{
			name:      "delete without retain",
			fields:    keep()[:1],
			reserved:  []int{3},
			wantCode:  CodeNumberNotRetained,
			wantFirst: 2,
		},
		{
			name:      "revoke historical reserved",
			fields:    keep(),
			reserved:  nil,
			wantCode:  CodeReservedRevoked,
			wantFirst: 3,
		},
		{
			name:      "reuse reserved number",
			fields:    append(keep(), Field{Name: "impostor", Number: 3, WireType: WireVarint}),
			reserved:  []int{3},
			wantCode:  CodeReservedReused,
			wantFirst: 3,
		},
		{
			name:      "field collides with own reserved list",
			fields:    append(keep(), Field{Name: "delta", Number: 5, WireType: WireVarint}),
			reserved:  []int{3, 5},
			wantCode:  CodeReservedReused,
			wantFirst: 5,
		},
		{
			name: "rename existing number",
			fields: []Field{
				{Name: "renamed", Number: 1, WireType: WireVarint},
				{Name: "beta", Number: 2, WireType: WireFixed64},
			},
			reserved:  []int{3},
			wantCode:  CodeFieldRenamed,
			wantFirst: 1,
		},
		{
			name: "change wire type",
			fields: []Field{
				{Name: "alpha", Number: 1, WireType: WireVarint},
				{Name: "beta", Number: 2, WireType: WireFixed32},
			},
			reserved:  []int{3},
			wantCode:  CodeWireTypeChanged,
			wantFirst: 2,
		},
		{
			name:      "not-retained wins over rename",
			fields:    []Field{{Name: "renamed", Number: 1, WireType: WireVarint}},
			reserved:  []int{3},
			wantCode:  CodeNumberNotRetained,
			wantFirst: 2,
		},
		{
			name:      "lowest violating number reported",
			fields:    nil, // drops 1 and 2
			reserved:  []int{3},
			wantCode:  CodeNumberNotRetained,
			wantFirst: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &SubmitRequest{RequestID: "r", Version: 3, Fields: tc.fields, ReservedNumbers: tc.reserved}
			err := validateEvolution(twoVersionSubject(), req)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %s, got nil", tc.wantCode)
			}
			if err.Code != tc.wantCode {
				t.Fatalf("expected code %s, got %s (%s)", tc.wantCode, err.Code, err.Message)
			}
			if err.FirstViolation == nil || *err.FirstViolation != tc.wantFirst {
				t.Fatalf("expected firstViolation %d, got %v", tc.wantFirst, err.FirstViolation)
			}
		})
	}
}

func TestValidateEvolutionFirstVersion(t *testing.T) {
	// No current state: anything goes, except a field that is also reserved.
	req := &SubmitRequest{
		RequestID:       "r",
		Version:         1,
		Fields:          []Field{{Name: "a", Number: 1, WireType: WireVarint}},
		ReservedNumbers: []int{1},
	}
	err := validateEvolution(nil, req)
	if err == nil || err.Code != CodeReservedReused {
		t.Fatalf("expected RESERVED_NUMBER_REUSED, got %v", err)
	}
	if err.FirstViolation == nil || *err.FirstViolation != 1 {
		t.Fatalf("expected firstViolation 1, got %v", err.FirstViolation)
	}

	req.ReservedNumbers = []int{9}
	if err := validateEvolution(nil, req); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}
