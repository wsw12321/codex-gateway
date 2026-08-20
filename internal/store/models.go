package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	UserRoleOwner  = "owner"
	UserRoleMember = "member"

	StatusActive   = "active"
	StatusDisabled = "disabled"
	StatusRevoked  = "revoked"

	InvitationOwnerBootstrap = "owner_bootstrap"
	InvitationMember         = "member"
	InvitationRecovery       = "recovery"
)

type User struct {
	ID             string
	Username       string
	DisplayName    string
	WebAuthnUserID []byte
	Role           string
	Status         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DisabledAt     *time.Time
	LastLoginAt    *time.Time
}

type Invitation struct {
	ID           string
	Kind         string
	TokenHash    []byte
	InviterID    *string
	TargetUserID *string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	UsedAt       *time.Time
	UsedByUserID *string
	RevokedAt    *time.Time
	SourceIP     *string
}

type WebAuthnCredential struct {
	ID             string
	UserID         string
	CredentialID   []byte
	CredentialJSON []byte
	SignCount      uint64
	Transports     []string
	BackupEligible bool
	BackupState    bool
	Discoverable   bool
	AAGUID         *string
	Nickname       string
	CreatedAt      time.Time
	LastUsedAt     *time.Time
}

// PasswordCredential contains a password verifier and must never be serialized
// into an HTTP response, audit event, or log record.
type PasswordCredential struct {
	UserID      string
	EncodedHash string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LastUsedAt  *time.Time
}

type LoginMethods struct {
	Passkey  bool `json:"passkey"`
	Password bool `json:"password"`
}

type Session struct {
	ID                 string
	UserID             string
	TokenHash          []byte
	CSRFSecret         []byte
	SourceIP           *string
	UserAgentHash      []byte
	CreatedAt          time.Time
	LastSeenAt         time.Time
	IdleExpiresAt      time.Time
	AbsoluteExpiresAt  time.Time
	RecentlyVerifiedAt *time.Time
	RevokedAt          *time.Time
	RevokeReason       string
}

type Device struct {
	ID         string
	UserID     string
	Name       string
	Status     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastSeenAt *time.Time
	DisabledAt *time.Time
}

type Project struct {
	ID         string
	UserID     string
	Slug       string
	Name       string
	Status     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ArchivedAt *time.Time
}

type APIKey struct {
	ID                 string
	PublicID           string
	KeyPrefix          string
	KeyHash            []byte
	UserID             string
	DeviceID           string
	DefaultProjectID   *string
	Name               string
	Status             string
	ModelAllowlist     []string
	RPMLimit           *int
	ConcurrentLimit    *int
	DailyRequestLimit  *int
	DailyTokenLimit    *int64
	CreatedAt          time.Time
	ExpiresAt          time.Time
	LastUsedAt         *time.Time
	RevokedAt          *time.Time
	RevokeReason       string
	RotatedFromID      *string
	UserStatus         string
	DeviceStatus       string
	DefaultProjectSlug *string
}

type UsageRequest struct {
	ID                      int64
	RequestID               string
	UserID                  string
	DeviceID                string
	APIKeyID                string
	KeyPrefix               string
	ProjectID               *string
	Model                   string
	RequestedModel          *string
	RequestedServiceTier    *string
	ActualServiceTier       *string
	Endpoint                string
	State                   string
	HTTPStatus              *int
	ErrorCode               *string
	RequestedAt             time.Time
	FirstTokenAt            *time.Time
	CompletedAt             *time.Time
	TTFTMillis              *int64
	DurationMillis          *int64
	InputTokens             int64
	CachedInputTokens       int64
	CacheWriteTokens        int64
	CacheWriteTokensPresent bool
	OutputTokens            int64
	ReasoningTokens         int64
	RequestBytes            int64
	ResponseBytes           int64
	UpstreamRequestID       *string
	PricingRuleVersion      int
	PricingServiceTier      *string
	ContextClass            *string
	PricingFallbackReason   *string
}

// TotalTokens follows the gateway accounting rule: cached input is already a
// subset of input and reasoning is already represented by output.
func (u UsageRequest) TotalTokens() int64 { return u.InputTokens + u.OutputTokens }

type DailyUsage struct {
	Day               time.Time
	UserID            string
	DeviceID          string
	APIKeyID          string
	ProjectID         *string
	Model             string
	Endpoint          string
	StatusClass       int
	ErrorCode         *string
	RequestCount      int64
	ErrorCount        int64
	InputTokens       int64
	CachedInputTokens int64
	CacheWriteTokens  int64
	OutputTokens      int64
	ReasoningTokens   int64
	RequestBytes      int64
	ResponseBytes     int64
	TTFTCount         int64
	TTFTSumMillis     int64
	P95TTFTMillis     *int64
	DurationCount     int64
	DurationSumMillis int64
	P95DurationMillis *int64
	UpdatedAt         time.Time
}

// MonthlyUsage is a long-lived metadata aggregate. Month is always the first
// calendar day in the configured aggregation timezone.
type MonthlyUsage struct {
	Month             time.Time
	UserID            string
	DeviceID          string
	APIKeyID          string
	ProjectID         *string
	Model             string
	Endpoint          string
	StatusClass       int
	ErrorCode         *string
	RequestCount      int64
	ErrorCount        int64
	InputTokens       int64
	CachedInputTokens int64
	CacheWriteTokens  int64
	OutputTokens      int64
	ReasoningTokens   int64
	RequestBytes      int64
	ResponseBytes     int64
	P95TTFTMillis     *int64
	P95DurationMillis *int64
	UpdatedAt         time.Time
}

type UsageSummary struct {
	RequestCount      int64
	ErrorCount        int64
	InputTokens       int64
	CachedInputTokens int64
	CacheWriteTokens  int64
	OutputTokens      int64
	ReasoningTokens   int64
	RequestBytes      int64
	ResponseBytes     int64
	P95TTFTMillis     int64
	P95DurationMillis int64
}

type GlobalUsageRow struct {
	UserID            string
	Username          string
	DisplayName       string
	Model             string
	RequestCount      int64
	InputTokens       int64
	CachedInputTokens int64
	CacheWriteTokens  int64
	OutputTokens      int64
	ReasoningTokens   int64
	ActualCostUSD     string
	ChargedUSD        string
	UncoveredUSD      string
	LedgerTokens      int64
}

type GlobalPricingBreakdownRow struct {
	Dimension        string
	Value            string
	RequestCount     int64
	CacheWriteTokens int64
	ActualCostUSD    string
}

type AuditEvent struct {
	ID             int64
	OccurredAt     time.Time
	ActorUserID    *string
	ActorSessionID *string
	ActorAPIKeyID  *string
	EventType      string
	Severity       string
	Success        bool
	SourceIP       *string
	SubjectType    string
	SubjectID      string
	RequestID      *string
	Metadata       map[string]any
}

type Alert struct {
	ID              int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Type            string
	Severity        string
	Status          string
	UserID          *string
	RequestID       *string
	DedupeKey       *string
	Title           string
	Details         map[string]any
	OccurrenceCount int64
	LastOccurredAt  time.Time
	AcknowledgedAt  *time.Time
	AcknowledgedBy  *string
	ResolvedAt      *time.Time
}

func marshalStringArray(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	b, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("%w: encode string array", ErrInvalid)
	}
	return string(b), nil
}

func unmarshalStringArray(raw []byte, dst *[]string) error {
	if len(raw) == 0 || string(raw) == "null" {
		*dst = []string{}
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode string array: %w", err)
	}
	return nil
}

const maxMetadataBytes = 32 << 10

var forbiddenMetadataKeys = []string{
	"authorization", "cookie", "prompt", "request_body", "response_body",
	"api_key", "access_token", "refresh_token", "oauth_token", "secret",
	"source_code", "response_text", "password", "encoded_hash", "phc",
}

func marshalSafeMetadata(value map[string]any) ([]byte, error) {
	if value == nil {
		value = map[string]any{}
	}
	if err := validateMetadataValue(value, 0); err != nil {
		return nil, err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: metadata is not JSON encodable", ErrInvalid)
	}
	if len(b) > maxMetadataBytes {
		return nil, fmt.Errorf("%w: metadata exceeds %d bytes", ErrInvalid, maxMetadataBytes)
	}
	return b, nil
}

func validateMetadataValue(value any, depth int) error {
	if depth > 8 {
		return fmt.Errorf("%w: metadata nesting is too deep", ErrInvalid)
	}
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			lower := strings.ToLower(key)
			for _, forbidden := range forbiddenMetadataKeys {
				if strings.Contains(lower, forbidden) {
					return fmt.Errorf("%w: metadata key %q is forbidden", ErrInvalid, key)
				}
			}
			if err := validateMetadataValue(child, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := validateMetadataValue(child, depth+1); err != nil {
				return err
			}
		}
	case nil, bool, string, float64, float32,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, json.Number:
		return nil
	default:
		return fmt.Errorf("%w: unsupported metadata value %T", ErrInvalid, value)
	}
	return nil
}

func decodeMetadata(raw []byte) (map[string]any, error) {
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode metadata: %w", err)
	}
	if result == nil {
		result = map[string]any{}
	}
	return result, nil
}
