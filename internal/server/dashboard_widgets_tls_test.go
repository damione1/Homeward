package server

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Only the explicitly isolated review extends its synthetic leaf certificate.
// The production TLS verifier and all ordinary one-hour fixtures are unchanged.
func dashboardWidgetsReviewCertificate(t *testing.T, dir string, deadline time.Time) *x509.CertPool {
	t.Helper()
	return nha181CertificateFixtureFor(t, dir, time.Now().Add(-time.Minute), deadline.Add(5*time.Minute))
}

func TestDashboardWidgetsReviewCertificateCoversBoundedReview(t *testing.T) {
	for _, lifetime := range []time.Duration{20 * time.Minute, 12 * time.Hour} {
		t.Run(lifetime.String(), func(t *testing.T) {
			dir := t.TempDir()
			deadline := time.Now().Add(lifetime)
			roots := dashboardWidgetsReviewCertificate(t, dir, deadline)
			raw, err := os.ReadFile(filepath.Join(dir, nha181CertificateName))
			if err != nil {
				t.Fatal(err)
			}
			block, _ := pem.Decode(raw)
			if block == nil {
				t.Fatal("missing review certificate")
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			options := x509.VerifyOptions{Roots: roots, DNSName: "127.0.0.1", CurrentTime: deadline, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			if _, err = cert.Verify(options); err != nil {
				t.Fatal("review expires before its declared deadline", err)
			}
			options.CurrentTime = deadline.Add(6 * time.Minute)
			if _, err = cert.Verify(options); err == nil {
				t.Fatal("expired review certificate was accepted")
			}
		})
	}
}
