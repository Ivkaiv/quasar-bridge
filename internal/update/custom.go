package update

import (
	"errors"
	"strings"
)

// CustomBuild is set at build time for installations containing local features.
// It is deliberately separate from semantic version parsing: restarting and
// emergency recovery remain available, while installing a stock release must
// never silently discard the local patches or make their configuration invalid.
var CustomBuild = "ivkaiv"

func (u *Updater) customBuild() bool {
	return CustomBuild != "" || strings.Contains(u.currentVersion, "-ivkaiv")
}

func (u *Updater) allowInstall() error {
	if u.customBuild() {
		return errors.New("custom build contains local features; automatic stock updates are disabled, port local changes before updating manually")
	}
	return nil
}
