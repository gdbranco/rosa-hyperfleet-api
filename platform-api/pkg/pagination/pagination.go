package pagination

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	hyperfleetdb "github.com/openshift-online/rosa-hyperfleet-api/hyperfleet-db"
)

const (
	defaultLimit = 50
	maxLimit     = 100
)

// Options is the standard pagination input for list operations.
type Options struct {
	Limit    int
	Continue string // opaque cursor token; empty = start from beginning
}

// ParseOptions extracts limit and continue from the HTTP request query string.
// Invalid or out-of-range limit values fall back to the default.
func ParseOptions(r *http.Request) Options {
	opts := Options{Limit: defaultLimit}

	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= maxLimit {
			opts.Limit = n
		}
	}

	opts.Continue = r.URL.Query().Get("continue")
	return opts
}

// Response is the standard envelope for all paginated list endpoints.
type Response[T any] struct {
	Items    []T    `json:"items"`
	Limit    int    `json:"limit"`
	HasMore  bool   `json:"has_more"`
	Continue string `json:"continue,omitempty"`
}

// platformToken wraps the hyperfleet-db continue token with an account ID so
// cross-account cursor reuse is caught at the platform-api layer.
type platformToken struct {
	Cursor    string `json:"cursor"`
	AccountID string `json:"account_id"`
}

// ErrInvalidContinueToken is returned when the continue token provided by a
// caller is malformed or belongs to a different account.
var ErrInvalidContinueToken = errors.New("invalid continue token")

// DecodeContinue validates the platform-level continue token for the given
// account and returns the inner hyperfleet-db cursor. An empty token is valid
// and returns an empty inner cursor (start from beginning).
func DecodeContinue(token, accountID string) (string, error) {
	if token == "" {
		return "", nil
	}
	data, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidContinueToken, err)
	}
	var pt platformToken
	if err := json.Unmarshal(data, &pt); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidContinueToken, err)
	}
	if pt.AccountID != accountID {
		return "", fmt.Errorf("%w: token account mismatch", ErrInvalidContinueToken)
	}
	return pt.Cursor, nil
}

// EncodeContinue wraps a hyperfleet-db cursor with the account ID to produce
// the platform-level continue token returned to callers.
func EncodeContinue(cursor, accountID string) string {
	if cursor == "" {
		return ""
	}
	data, _ := json.Marshal(platformToken{Cursor: cursor, AccountID: accountID})
	return base64.StdEncoding.EncodeToString(data)
}

// IsInvalidCursor reports whether err is a continue token error (hyperfleet-db
// or platform-level), suitable for mapping to HTTP 400.
func IsInvalidCursor(err error) bool {
	return errors.Is(err, ErrInvalidContinueToken) ||
		errors.Is(err, hyperfleetdb.ErrInvalidContinueToken)
}
