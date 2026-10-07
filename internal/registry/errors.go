package registry

// Stable error codes returned by the API. These codes are part of the
// public contract and must not change.
const (
	CodeInvalidRequest     = "INVALID_REQUEST"
	CodeSubjectNotFound    = "SUBJECT_NOT_FOUND"
	CodeVersionConflict    = "VERSION_CONFLICT"
	CodeRequestIDConflict  = "REQUEST_ID_CONFLICT"
	CodeEvolutionViolation = "SCHEMA_EVOLUTION_VIOLATION"
	CodeInternal           = "INTERNAL"
)

// Evolution violation rules, reported inside SCHEMA_EVOLUTION_VIOLATION
// responses to identify exactly what was rejected.
const (
	RuleFieldRenamed    = "FIELD_RENAMED"
	RuleWireTypeChanged = "WIRE_TYPE_CHANGED"
	RuleNumberReserved  = "NUMBER_RESERVED"
)

// Violation describes one rejected field number.
type Violation struct {
	Number  int    `json:"number"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// ErrorBody is the machine-readable error payload of every non-2xx response.
type ErrorBody struct {
	Code            string      `json:"code"`
	Message         string      `json:"message"`
	CurrentVersion  *int        `json:"currentVersion,omitempty"`
	ViolatingNumber *int        `json:"violatingNumber,omitempty"`
	Violations      []Violation `json:"violations,omitempty"`
}

// ErrorResponse wraps ErrorBody in the top-level "error" key.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}
