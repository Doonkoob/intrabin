package monitoring

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"

	"github.com/intrabin/intrabin/internal/model"
)

// Classify maps a Go network error to a precise error category.
// It never produces http_5xx: that category is only set from a real HTTP response.
func Classify(err error) model.ErrorCategory {
	if err == nil {
		return model.ErrNone
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsTimeout {
			return model.ErrTimeout
		}
		return model.ErrDNS
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return model.ErrTimeout
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return model.ErrTimeout
	}
	var certInv x509.CertificateInvalidError
	var unkAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var verr *tls.CertificateVerificationError
	var rhe tls.RecordHeaderError
	var alert tls.AlertError
	if errors.As(err, &certInv) || errors.As(err, &unkAuth) || errors.As(err, &hostErr) || errors.As(err, &verr) || errors.As(err, &rhe) || errors.As(err, &alert) {
		return model.ErrTLS
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return model.ErrConnectionRefused
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.EHOSTDOWN):
		return model.ErrNetworkUnreachable
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return model.ErrConnectionReset
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "tls:") || strings.Contains(msg, "x509:") || strings.Contains(msg, "certificate"):
		return model.ErrTLS
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded"):
		return model.ErrTimeout
	case strings.Contains(msg, "connection refused"):
		return model.ErrConnectionRefused
	case strings.Contains(msg, "no route to host") || strings.Contains(msg, "network is unreachable"):
		return model.ErrNetworkUnreachable
	case strings.Contains(msg, "connection reset") || strings.Contains(msg, "broken pipe"):
		return model.ErrConnectionReset
	case strings.Contains(msg, "no such host"):
		return model.ErrDNS
	}
	return model.ErrUnknown
}
