package meridian

// Engine error codes: the engine's closed list, in the engine's order.
const (
	CodeInvalidRequest          = "invalid_request"
	CodeInvalidAPIKey           = "invalid_api_key"
	CodeKeyRevoked              = "key_revoked"
	CodeKeyExpired              = "key_expired"
	CodeProjectUnavailable      = "project_unavailable"
	CodeOrganisationUnavailable = "organisation_unavailable"
	CodeModelNotAllowed         = "model_not_allowed"
	CodeBYOKRequired            = "byok_required"
	CodeProjectHostMismatch     = "project_host_mismatch"
	CodeModelNotFound           = "model_not_found"
	CodeRouteNotFound           = "route_not_found"
	CodeMethodNotAllowed        = "method_not_allowed"
	CodeCapabilityUnsupported   = "capability_unsupported"
	CodeBodyTooLarge            = "body_too_large"
	CodeRateLimited             = "rate_limited"
	CodeInsufficientCredit      = "insufficient_credit"
	CodeHardLimitReached        = "hard_limit_reached"
	CodeEntitlementMissing      = "entitlement_missing"
	CodeIdempotencyConflict     = "idempotency_conflict"
	CodeUpstreamUnavailable     = "upstream_unavailable"
	CodeUpstreamError           = "upstream_error"
	CodeDeadlineExceeded        = "deadline_exceeded"
	CodeServiceUnavailable      = "service_unavailable"
	CodeInternal                = "internal"
	CodeInvalidToken            = "invalid_token"
	CodePermissionDenied        = "permission_denied"
	CodeNotFound                = "not_found"
	CodeConflict                = "conflict"
)

// SDK-only codes. The SDK adds no others.
const (
	CodeNetworkError = "network_error"
	CodeAborted      = "aborted"
)

// Error types: the engine's five, and client_error for failures inside the SDK.
const (
	TypeInvalidRequest = "invalid_request_error"
	TypeAuthentication = "authentication_error"
	TypePermission     = "permission_error"
	TypeRateLimit      = "rate_limit_error"
	TypeAPI            = "api_error"
	TypeClient         = "client_error"
)

var engineCodes = []string{
	CodeInvalidRequest, CodeInvalidAPIKey, CodeKeyRevoked, CodeKeyExpired, CodeProjectUnavailable,
	CodeOrganisationUnavailable, CodeModelNotAllowed, CodeBYOKRequired, CodeProjectHostMismatch,
	CodeModelNotFound, CodeRouteNotFound, CodeMethodNotAllowed, CodeCapabilityUnsupported, CodeBodyTooLarge,
	CodeRateLimited, CodeInsufficientCredit, CodeHardLimitReached, CodeEntitlementMissing,
	CodeIdempotencyConflict, CodeUpstreamUnavailable, CodeUpstreamError, CodeDeadlineExceeded,
	CodeServiceUnavailable, CodeInternal, CodeInvalidToken, CodePermissionDenied, CodeNotFound, CodeConflict,
}

// EngineCodes returns the engine's 28 error codes in the engine's order.
func EngineCodes() []string { return append([]string(nil), engineCodes...) }

// EngineTypes returns the engine's five error types.
func EngineTypes() []string {
	return []string{TypeInvalidRequest, TypeAuthentication, TypePermission, TypeRateLimit, TypeAPI}
}

// engineStatus is the HTTP status the engine sends with each code (meridian-domain error.rs).
// It fills Status for errors that arrive inside a stream, where no HTTP status exists.
var engineStatus = map[string]int{
	CodeInvalidRequest: 400, CodeCapabilityUnsupported: 400,
	CodeInvalidAPIKey: 401, CodeKeyRevoked: 401, CodeKeyExpired: 401, CodeInvalidToken: 401,
	CodeInsufficientCredit: 402,
	CodeProjectUnavailable: 403, CodeOrganisationUnavailable: 403, CodeModelNotAllowed: 403,
	CodeBYOKRequired: 403, CodeProjectHostMismatch: 403, CodePermissionDenied: 403, CodeEntitlementMissing: 403,
	CodeModelNotFound: 404, CodeRouteNotFound: 404, CodeNotFound: 404,
	CodeMethodNotAllowed: 405, CodeIdempotencyConflict: 409, CodeConflict: 409, CodeBodyTooLarge: 413,
	CodeRateLimited: 429, CodeHardLimitReached: 429, CodeInternal: 500,
	CodeUpstreamUnavailable: 502, CodeUpstreamError: 502, CodeServiceUnavailable: 503, CodeDeadlineExceeded: 504,
}

var retryableCodes = map[string]bool{
	CodeRateLimited: true, CodeUpstreamUnavailable: true, CodeServiceUnavailable: true,
	CodeDeadlineExceeded: true, CodeNetworkError: true,
}
