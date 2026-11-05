package checks

import (
	"fmt"
	"strings"
)

// CheckAPIConnection is the name of the check below. The engine knows it: this
// check keeps running when all the others have no fresh data to look at.
const CheckAPIConnection = "api-connection"

// APIConnection reports that smokealarm cannot keep its copy of the cluster up
// to date. That is a problem because the other checks would go on reading the
// old copy, and a smoke alarm must say when it cannot see anything. The kube
// package notices it and only fills Snapshot.APIProblem once it has lasted a
// while, so a restart of the API server is no alarm. Severity: critical.
func APIConnection(snap *Snapshot, _ Options) []Finding {
	problem := snap.APIProblem
	if problem == nil {
		return nil
	}
	ref := ObjectRef{Kind: KindAPIServer, Name: "kubernetes"}
	return []Finding{newFinding(CheckAPIConnection, Critical, ref, "APIConnectionFailing", apiProblemMessage(problem))}
}

func apiProblemMessage(problem *APIProblem) string {
	what := "The Kubernetes API cannot be read, the other checks are paused and their problems stay as they are"
	if !problem.Blind {
		what = "Cannot read " + strings.Join(problem.Resources, ", ") + " from the Kubernetes API, findings about them may be out of date"
	}
	return fmt.Sprintf("%s. Last error: %s", what, problem.Error)
}
