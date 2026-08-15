package persistence_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
)

func TestPublicInputsAreBoundedAndErrorsAreCategorical(t *testing.T) {
	unsafe := "password=hunter2"
	input := persistence.ProcessInput{
		Scope: persistence.Scope{Region: unsafe, TenantID: "tenant-a"},
	}

	_, err := (*persistence.Store)(nil).Process(context.Background(), input)
	if !errors.Is(err, persistence.ErrInvalidInput) {
		t.Fatalf("want categorical invalid input, got %v", err)
	}
	if strings.Contains(err.Error(), unsafe) {
		t.Fatalf("error leaked unsafe input: %v", err)
	}
}
