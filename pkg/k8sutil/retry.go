/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// The original source of this file is https://github.com/kubernetes/kubernetes/blob/ef0c9f0c5b8efbba948a0be2c98d9d2e32e0b68c/pkg/client/unversioned/util.go

package k8sutil

import (
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
)

// DefaultRetry is the recommended retry for a conflict where multiple clients
// are making changes to the same resource.
var DefaultRetry = wait.Backoff{
	Steps:    5,
	Duration: 10 * time.Millisecond,
	Factor:   1.0,
	Jitter:   0.1,
}

// DefaultBackoff is the recommended backoff for a conflict where a client
// may be attempting to make an unrelated modification to a resource under
// active management by one or more controllers.
var DefaultBackoff = wait.Backoff{
	Steps:    4,
	Duration: 10 * time.Millisecond,
	Factor:   5.0,
	Jitter:   0.1,
}

// RetryOnConflict executes the provided function repeatedly, retrying if the server returns a conflicting
// write. Callers should preserve previous executions if they wish to retry changes. It performs an
// exponential backoff.
//
//     var pod *api.Pod
//     err := RetryOnConflict(DefaultBackoff, func() (err error) {
//       pod, err = c.Pods("mynamespace").UpdateStatus(podStatus)
//       return
//     })
//     if err != nil {
//       // may be conflict if max retries were hit
//       return err
//     }
//     ...
//
// TODO: Make Backoff an interface?
func RetryOnConflict(backoff wait.Backoff, fn func() error) error {
	var lastConflictErr error

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		err := fn()

		switch {
		case err == nil:
			return true, nil
		case errors.IsConflict(err):
			lastConflictErr = err

			return false, nil
		default:
			return false, err
		}
	})

	if err == wait.ErrWaitTimeout {
		err = lastConflictErr
	}

	return err
}

// RetryOnError retries a function repeatedly with the specified backoff until it succeeds or times out
func RetryOnError(backoff wait.Backoff, fn func() error) error {
	var lastErr error

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		lastErr := fn()

		return lastErr == nil, nil
	})

	if err == wait.ErrWaitTimeout {
		err = lastErr
	}

	return err
}
