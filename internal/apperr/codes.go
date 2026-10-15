package apperr

// The error catalog of the spec (DOMAIN.md section 6). A code is never reused for another meaning.
const (
	CodeInvalidRequest       = "VALIDATION-01"
	CodeOrderNotEditable     = "VALIDATION-02"
	CodeOrderEmpty           = "VALIDATION-03"
	CodeInvalidTransition    = "VALIDATION-04"
	CodeInvalidFilter        = "VALIDATION-05"
	CodeUnknownSort          = "VALIDATION-06"
	CodeCustomerNotFoundBody = "VALIDATION-07"
	CodeAlreadyCanceled      = "VALIDATION-08"
	CodeItemLimit            = "VALIDATION-09"

	CodeBodyTooLarge = "VALIDATION-12"

	// Additions of this implementation: the spec leaves unmatched routes and methods to the framework default,
	// this API answers them in the same error envelope as everything else.
	CodeMethodNotAllowed = "VALIDATION-13"
	CodeRouteNotFound    = "RESOURCE-NOT-FOUND-00"

	CodeCustomerNotFound = "RESOURCE-NOT-FOUND-01"
	CodeOrderNotFound    = "RESOURCE-NOT-FOUND-02"
	CodeItemNotFound     = "RESOURCE-NOT-FOUND-03"

	CodeConcurrentModification = "CONFLICT-00"
	CodeDuplicateTaxID         = "CONFLICT-01"
	CodeDuplicatePassport      = "CONFLICT-02"
	CodeCustomerHasOrders      = "CONFLICT-03"
	CodeDataIntegrity          = "CONFLICT-04"

	CodeUnauthenticated = "AUTH-01"
	CodeForbidden       = "AUTH-02"

	CodeRateLimit = "RATE-00"
	CodeInternal  = "INTERNAL-00"
)
