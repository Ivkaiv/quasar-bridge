package bridge

import (
	"context"
	"strings"

	device "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/common/device_state"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	"google.golang.org/protobuf/proto"

	"quasarbridge/internal/playback"
	"quasarbridge/internal/sound"
)

// Native streams still belong to Alice, never to the bridge's private queue.
// A paused stream alone can be stale: do not steal a continuing local dialogue.
func nativePlaybackCommand(ctx context.Context, text string, dialogueActive bool) bool {
	if playback.Command(text) == "" {
		return false
	}
	// "Включи Назад" may name a home device; keep such starts on the normal path.
	lower := strings.ToLower(text)
	if strings.Contains(lower, "включи") || strings.Contains(lower, "выключи") {
		return false
	}
	state := sound.CurrentPlayback(ctx)
	if !state.Present || state.StreamID == "" {
		return false
	}
	return state.PlayerState == "Playing" || state.PlayerState == "Buffering" ||
		(state.PlayerState == "Paused" && !dialogueActive)
}

func nativePlaybackState(req *request.TAliceRequestProto) sound.PlaybackState {
	r := req.GetRequest()
	state := r.GetDeviceState()
	if state == nil {
		raw := r.GetDeviceStateRaw()
		if len(raw) == 0 || len(raw) > 1<<20 {
			return sound.PlaybackState{}
		}
		state = new(device.TDeviceState)
		if proto.Unmarshal(raw, state) != nil {
			return sound.PlaybackState{}
		}
	}
	p := state.GetAudioPlayer()
	return sound.PlaybackState{Present: true, OffsetKnown: p != nil && p.OffsetMs != nil, StreamID: p.GetCurrentlyPlaying().GetStreamId(), OffsetMS: max(0, int64(p.GetOffsetMs())), PlayerState: p.GetPlayerState().String()}
}
