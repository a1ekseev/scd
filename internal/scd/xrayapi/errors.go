package xrayapi

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/a1ekseev/scd/internal/common/neterr"
)

// Error is the single error type returned by every Client RPC, the Go
// counterpart of the TS ApiRequestError. Its text ends up in sync reports,
// logs and the status API, so it is kept verbatim (02 §2.7, D2).
type Error struct {
	// Message is the user-visible error text.
	Message string
	// Unavailable reports that the Xray API did not answer (gRPC
	// Unavailable or DeadlineExceeded), not that it refused the call.
	Unavailable bool
}

// Error implements error.
func (e *Error) Error() string { return e.Message }

// IsUnavailable reports whether err is an *Error of an Xray API that did
// not answer.
func IsUnavailable(err error) bool {
	e, ok := errors.AsType[*Error](err)
	return ok && e.Unavailable
}

// mapError converts a gRPC call error into *Error (02 §2.7):
//   - Unavailable mentioning "connection refused"/"operation not permitted"
//     (ECONNREFUSED/EPERM) → "Unable to connect to Xray API at …";
//   - DeadlineExceeded → "Request to Xray API at … timed out after Nms.";
//   - otherwise the status message (Xray texts such as
//     "app/proxyman/outbound: existing tag found: x") without the local
//     address of a network error (neterr.StableText), or
//     "gRPC call failed with status <code>" when it is empty.
//
// Unavailable is set for every Unavailable and DeadlineExceeded status.
func mapError(err error, address string, timeout time.Duration) error {
	if err == nil {
		return nil
	}
	st := status.Convert(err)
	msg := st.Message()
	down := st.Code() == codes.Unavailable || st.Code() == codes.DeadlineExceeded
	switch {
	case st.Code() == codes.Unavailable &&
		(strings.Contains(msg, "connection refused") || strings.Contains(msg, "operation not permitted")):
		return &Error{Message: "Unable to connect to Xray API at " + address +
			`. Start the Xray service first, for example with "docker compose up -d".`, Unavailable: true}
	case st.Code() == codes.DeadlineExceeded:
		return &Error{Message: "Request to Xray API at " + address + " timed out after " +
			strconv.FormatInt(timeout.Milliseconds(), 10) + "ms.", Unavailable: true}
	case msg != "":
		return &Error{Message: neterr.StableText(msg), Unavailable: down}
	default:
		return &Error{Message: "gRPC call failed with status " + strconv.Itoa(int(st.Code())), Unavailable: down}
	}
}
