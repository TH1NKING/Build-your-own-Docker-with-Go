//go:build !linux

package profilebundle

import "errors"

func Install(InstallOptions) (string, error) {
	return "", errors.New("root-owned Profile Bundle installation is supported only on Linux")
}
