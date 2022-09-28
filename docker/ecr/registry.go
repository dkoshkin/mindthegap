// Copyright 2021 D2iQ, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ecr

import (
	"regexp"
)

var ecrRegistryRegexp = regexp.MustCompile(
	`^(?:https://)?[a-zA-Z0-9]+\.dkr\.ecr\.[^.]+\.amazonaws\.com/?`,
)

func IsECRRegistry(registryAddress string) bool {
	return ecrRegistryRegexp.MatchString(registryAddress)
}
