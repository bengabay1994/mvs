package adapter

import (
	"fmt"

	"github.com/bengabay1994/mvs/internal/fsutil"
	"github.com/bengabay1994/mvs/internal/session"
)

func finalizeReportDigests(report *session.Report) {
	if !report.OK {
		return
	}
	for i := range report.Applied {
		action := &report.Applied[i]
		if action.Target == "" {
			continue
		}
		var digest string
		var err error
		if action.Kind == "sqlite_exec" {
			digest, err = fsutil.SQLiteFingerprint(action.Target)
		} else {
			digest, err = fsutil.Fingerprint(action.Target)
		}
		if err != nil {
			report.OK = false
			report.Errors = append(report.Errors, fmt.Sprintf("fingerprint %s: %v", action.Target, err))
			return
		}
		action.Digest = digest
	}
}
