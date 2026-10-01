/*
Copyright 2026 Serge Logvinov.

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

package tools

import (
	"testing"
	"time"
)

// SetEventTimings shortens the event read timings for one test.
func SetEventTimings(t *testing.T, hello, idle time.Duration, fanOut int) {
	t.Helper()

	oldHello, oldIdle, oldFanOut := eventsHelloTimeout, eventsIdleTimeout, eventsFanOut
	eventsHelloTimeout, eventsIdleTimeout, eventsFanOut = hello, idle, fanOut

	t.Cleanup(func() { eventsHelloTimeout, eventsIdleTimeout, eventsFanOut = oldHello, oldIdle, oldFanOut })
}
