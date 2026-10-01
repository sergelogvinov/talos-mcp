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

package talostest

import (
	"encoding/base32"
	"encoding/binary"
	"testing"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// xidEncoding is the base32hex alphabet of github.com/rs/xid.
var xidEncoding = base32.NewEncoding("0123456789abcdefghijklmnopqrstuv").WithPadding(base32.NoPadding)

// EventID returns an xid, as machined uses for event IDs, for time at and
// counter seq.
func EventID(at time.Time, seq int) string {
	var id [12]byte

	binary.BigEndian.PutUint32(id[:4], uint32(at.Unix())) //nolint:gosec
	binary.BigEndian.PutUint32(id[8:], uint32(seq))       //nolint:gosec

	return xidEncoding.EncodeToString(id[:])
}

// Event returns a machined event as streamed by the Events API.
func Event(t *testing.T, at time.Time, seq int, actorID string, payload proto.Message) *machineapi.Event {
	t.Helper()

	data, err := proto.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	return &machineapi.Event{
		Id:      EventID(at, seq),
		ActorId: actorID,
		Data: &anypb.Any{
			TypeUrl: "talos/runtime/" + string(payload.ProtoReflect().Descriptor().FullName()),
			Value:   data,
		},
	}
}
