package ids_test

import (
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/ids"
)

func TestSystemSourceProducesValidIdentifiers(t *testing.T) {
	source := ids.System()
	for i := 0; i < 20; i++ {
		id, err := source.New()
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}
		if err := ids.Validate(id); err != nil {
			t.Fatalf("system source produced %s, which fails validation: %v", id, err)
		}
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr string
	}{
		{
			name: "canonical uuidv7",
			id:   "0194f0a0-0000-7000-8000-000000000001",
		},
		{
			name:    "uppercase text",
			id:      "0194F0A0-0000-7000-8000-000000000001",
			wantErr: "canonical",
		},
		{
			name:    "uuidv4",
			id:      "9f8b7c6d-5e4f-4a3b-8c9d-0e1f2a3b4c5d",
			wantErr: "version 7",
		},
		{
			name:    "not a uuid",
			id:      "record-1",
			wantErr: "not a uuid",
		},
		{
			name:    "empty",
			id:      "",
			wantErr: "not a uuid",
		},
		{
			// A UUID-shaped string wrapped in braces parses, but it is not the
			// canonical text a JSON boundary is allowed to carry.
			name:    "braced form",
			id:      "{0194f0a0-0000-7000-8000-000000000001}",
			wantErr: "canonical",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ids.Validate(test.id)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("want %s accepted, got %v", test.id, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want %s rejected because of %q, got no error", test.id, test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("want the error to mention %q, got %v", test.wantErr, err)
			}
		})
	}
}
