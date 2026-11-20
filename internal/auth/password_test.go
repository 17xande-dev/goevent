package auth

import (
	"strings"
	"testing"
)

func TestHashPasswordRejectsEmptyPassword(t *testing.T) {
	if _, err := HashPassword(t.Context(), ""); err == nil {
		t.Error("HashPassword accepted an empty password")
	}
}

func TestInvalidHashRecoveryHint(t *testing.T) {
	if err := validatePasswordHash("nonsense"); err == nil || !strings.Contains(err.Error(), "make hashpw") {
		t.Errorf("invalid hash error = %v, want the recovery command", err)
	}
}
