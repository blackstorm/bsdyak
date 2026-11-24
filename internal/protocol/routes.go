package protocol

import (
	"fmt"
	"net/url"
)

const (
	RouteHealth = "/v1/health"
)

func ProjectBase(project string) string {
	return "/v1/projects/" + url.PathEscape(project)
}

func RouteSyncManifest(project string) string {
	return ProjectBase(project) + "/sync/manifest"
}

func RouteSyncUpload(project string) string {
	return ProjectBase(project) + "/sync/upload"
}

func RouteSyncArtifacts(project string) string {
	return ProjectBase(project) + "/sync/artifacts"
}

func RouteDeploy(project string) string {
	return ProjectBase(project) + "/deploy"
}

func RouteDown(project string) string {
	return ProjectBase(project) + "/down"
}

func RoutePs(project string) string {
	return ProjectBase(project) + "/ps"
}

func RouteLogs(project string) string {
	return ProjectBase(project) + "/logs"
}

func RouteExec(project string) string {
	return ProjectBase(project) + "/exec"
}

func RouteSecrets(project string) string {
	return ProjectBase(project) + "/secrets"
}

// ParseProjectFromPath extracts the project name from /v1/projects/<name>/...
// Used by yak bridge --allow-project to isolate scoped CI keys.
func ParseProjectFromPath(path string) (string, error) {
	const prefix = "/v1/projects/"
	if len(path) <= len(prefix) || path[:len(prefix)] != prefix {
		return "", fmt.Errorf("path %q does not target a project", path)
	}
	rest := path[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			unescaped, err := url.PathUnescape(rest[:i])
			if err != nil {
				return "", err
			}
			return unescaped, nil
		}
	}
	unescaped, err := url.PathUnescape(rest)
	if err != nil {
		return "", err
	}
	return unescaped, nil
}
