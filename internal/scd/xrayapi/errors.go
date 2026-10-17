package xrayapi

import (
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Error is the single error type returned by every Client RPC, the Go
// counterpart of the TS ApiRequestError. Its text ends up in sync reports,
// logs and the status API, so it is kept verbatim (02 §2.7, D2).
type Error struct {
	// Message is the user-visible error text.
	Message string
}

// Error implements error.
func (e *Error) Error() string { return e.Message }

// mapError converts a gRPC call error into *Error (02 §2.7):
//   - Unavailable mentioning "connection refused"/"operation not permitted"
//     (ECONNREFUSED/EPERM) → "Unable to connect to Xray API at …";
//   - DeadlineExceeded → "Request to Xray API at … timed out after Nms.";
//   - otherwise the status message as is (Xray texts such as
//     "app/proxyman/outbound: existing tag found: x"), or
//     "gRPC call failed with status <code>" when it is empty.
func mapError(err error, address string, timeout time.Duration) error {
	if err == nil {
		return nil
	}
	st := status.Convert(err)
	msg := st.Message()
	switch {
	case st.Code() == codes.Unavailable &&
		(strings.Contains(msg, "connection refused") || strings.Contains(msg, "operation not permitted")):
		return &Error{Message: "Unable to connect to Xray API at " + address +
			`. Start the Xray service first, for example with "docker compose up -d".`}
	case st.Code() == codes.DeadlineExceeded:
		return &Error{Message: "Request to Xray API at " + address + " timed out after " +
			strconv.FormatInt(timeout.Milliseconds(), 10) + "ms."}
	case msg != "":
		return &Error{Message: msg}
	default:
		return &Error{Message: "gRPC call failed with status " + strconv.Itoa(int(st.Code()))}
	}
}
