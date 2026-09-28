package jsonrpc

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// errorName is the fixed "name" field every JSONRPC 1.1 error object in
// this codebase carries. Confirmed against two captured production
// errors, byte for byte (app_service/trc and Workspace/trc in the
// dev_container modules tree, both from real WorkspaceImpl.pm failures):
//
//	{"id":"206571088478999","error":{"name":"JSONRPCError",
//	 "error":"_ERROR_Path HASH(0x5652d5e97c60) does not include at least a
//	 top level directory!_ERROR_ at .../WorkspaceImpl.pm line 186.\n",
//	 "message":"_ERROR_Path HASH(0x5652d5e97c60) does not include at least
//	 a top level directory!_ERROR_","code":-32603},"version":"1.1"}
//
// Note what that trace does *not* contain: a "data" key. The plan
// document's paraphrase of the error shape includes one
// ({"name","code","message","error","data"}); the real wire format, per
// this capture and per the client stub's own error-reading code
// (typecomp/lib/Bio/KBase/KIDL/p3_client_stub.tt:180-183,
//
//	my $msg = $result->{error}->{error} || $result->{error}->{message};
//
// ) only ever reads "error" and "message". This package still carries a
// Data field (used by nothing today) since the plan explicitly calls it
// out and a future phase may want structured error detail, but no
// constructor here populates it, and it is omitted from the wire
// (json:",omitempty") whenever it's unset -- so today's wire output
// matches the captured trace exactly, not the plan's paraphrase.
const errorName = "JSONRPCError"

// Error is the JSONRPC 1.1 error envelope's "error" object.
//
// Detail (wire key "error") carries the full error detail and Message the
// short form -- mirroring the Perl side's message/$@ split described in
// the implementation plan, even though Go has no exact equivalent of
// stripping " at FILE line N." from a stringified exception. The client
// stub cited above prefers Detail over Message when both are present.
type Error struct {
	Name    string `json:"name"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"error,omitempty"`
	Data    any    `json:"data,omitempty"`
}

// ErrorEnvelope is the full JSONRPC 1.1 error response body.
type ErrorEnvelope struct {
	ErrorObj Error           `json:"error"`
	ID       json.RawMessage `json:"id"`
	Version  string          `json:"version"`
}

// NewParseError builds a -32700 error for a request body that didn't
// decode as JSON at all.
func NewParseError(cause error) *Error {
	msg := "Parse error"
	if cause != nil {
		msg = fmt.Sprintf("Parse error: %s", cause)
	}
	return &Error{Name: errorName, Code: -32700, Message: msg, Detail: msg}
}

// NewMethodNotFound builds a -32601 error for a method name Register
// never saw.
func NewMethodNotFound(method string) *Error {
	msg := fmt.Sprintf("%q is not a valid method in this service", method)
	return &Error{Name: errorName, Code: -32601, Message: msg, Detail: msg}
}

// NewInvalidParams builds a -32602 error for a params array a handler
// could not unmarshal into its expected shape.
func NewInvalidParams(cause error) *Error {
	msg := "Invalid params"
	if cause != nil {
		msg = fmt.Sprintf("Invalid params: %s", cause)
	}
	return &Error{Name: errorName, Code: -32602, Message: msg, Detail: msg}
}

// NewAuthRequiredMissing builds the -32603 error for an AuthRequired
// method called with no Authorization header.
//
// Message is copied verbatim -- not paraphrased -- from
// app_service/lib/Bio/KBase/AppService/AsyncService.pm:235
// (`"Authentication required for AppService but no authentication header
// was passed"`), which is a second, hand-maintained implementation of the
// exact same check p3_server_stub.tt's generated call_method performs
// (`$self->exception('PerlError', "Authentication required for
// [% module.module_name %] but no authentication header was passed")`);
// only the module name varies between generated services. moduleName here
// is the module portion Server.Register split off the registered method
// name (see splitMethod), which for every SRACache method is "SRACache".
func NewAuthRequiredMissing(moduleName string) *Error {
	msg := fmt.Sprintf("Authentication required for %s but no authentication header was passed", moduleName)
	return &Error{Name: errorName, Code: -32603, Message: msg, Detail: msg}
}

// NewAuthTokenInvalid builds the -32603 error for an AuthRequired method
// called with an Authorization header the Validator rejected.
//
// Message is copied verbatim from AsyncService.pm:243
// (`"Token validation failed: $validate_err"`), the same wording
// p3_server_stub.tt's call_method raises via
// `$self->exception('PerlError', "Token validation failed: $validate_err")`.
func NewAuthTokenInvalid(validateErr error) *Error {
	msg := fmt.Sprintf("Token validation failed: %s", validateErr)
	return &Error{Name: errorName, Code: -32603, Message: msg, Detail: msg}
}

// NewInternalError builds a -32603 error for anything else that goes
// wrong inside a handler (or inside the dispatcher itself, e.g. an
// arity mismatch).
func NewInternalError(cause error) *Error {
	msg := cause.Error()
	return &Error{Name: errorName, Code: -32603, Message: msg, Detail: msg}
}

// writeError sends err as the HTTP response. Every JSONRPC error in this
// codebase is HTTP 500 regardless of its JSONRPC code -- confirmed by the
// captured production trace above (a -32603 error served as
// "HTTP/1.1 500 Internal Server Error") and, independently, by
// app_service/lib/Bio/KBase/AppService/AsyncService.pm's error_return,
// which hardcodes 500 for every one of -32700/-32601/-32602/-32603 alike.
func writeError(w http.ResponseWriter, id json.RawMessage, err *Error) {
	writeJSON(w, http.StatusInternalServerError, ErrorEnvelope{
		ErrorObj: *err,
		ID:       id,
		Version:  "1.1",
	})
}
