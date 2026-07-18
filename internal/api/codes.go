// Governing: SPEC-0005 REQ "Standard Error Response Format", ADR-0008
package api

// Machine-readable error codes returned in the "code" field of every API
// error response. All codes are UPPER_SNAKE — clients switch on these values,
// so every writeError call site MUST use one of these constants (enforced by
// TestErrorCodes_AllWriteErrorCallSitesUseUpperSnakeConstants).
const (
	CodeUnauthorized          = "UNAUTHORIZED"
	CodeForbidden             = "FORBIDDEN"
	CodeNotFound              = "NOT_FOUND"
	CodeBadRequest            = "BAD_REQUEST"
	CodeInternalError         = "INTERNAL_ERROR"
	CodeInvalidSlug           = "INVALID_SLUG"
	CodeInvalidURL            = "INVALID_URL"
	CodeInvalidFieldLength    = "INVALID_FIELD_LENGTH"
	CodeInvalidVisibility     = "INVALID_VISIBILITY"
	CodeInvalidTagName        = "INVALID_TAG_NAME"
	CodeTooManyTags           = "TOO_MANY_TAGS"
	CodeSlugConflict          = "SLUG_CONFLICT"
	CodeDBBusy                = "DB_BUSY"
	CodeTagWriteFailed        = "TAG_WRITE_FAILED"
	CodeDuplicateOwner        = "DUPLICATE_OWNER"
	CodeDuplicateShare        = "DUPLICATE_SHARE"
	CodePrimaryOwnerProtected = "PRIMARY_OWNER_PROTECTED"
	CodeLLMNotConfigured      = "LLM_NOT_CONFIGURED"
	CodeLLMError              = "LLM_ERROR"
)
