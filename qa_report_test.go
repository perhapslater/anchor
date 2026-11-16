package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestQAReportRejectsInvalidCompleteBody(t *testing.T) {
	for name, body := range map[string]string{
		"unknown property": `{"app":"photos","unexpected":true}`,
		"second document":  `{"app":"photos"} {"app":"quiet"}`,
		"malformed suffix": `{"app":"photos"} garbage`,
		"oversized suffix": `{"app":"photos"}` + strings.Repeat(" ", 1024),
		"empty":            "",
	} {
		t.Run(name, func(t *testing.T) {
			h, notifier := newFeatureHandler(t, featureConfig)
			result := postReport(h, "mom@example.com", "photos", func(r *http.Request) {
				r.Body = io.NopCloser(strings.NewReader(body))
			})
			if result.Code != http.StatusBadRequest || notifier.count() != 0 {
				t.Fatalf("invalid complete body: status=%d notifications=%d; want 400 and no side effect", result.Code, notifier.count())
			}
		})
	}
}
