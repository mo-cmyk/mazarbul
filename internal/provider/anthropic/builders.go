package anthropic

// SystemPart is one segment of a system prompt. The leading contiguous
// run of Static==true parts is the cache-eligible prefix; everything
// from the first Static==false part onward is dynamic and ineligible.
// BuildCachedSystemPrompt attaches the ephemeral cache_control marker
// to the last part of that leading static prefix so every call that
// shares the prefix sees a cache hit.
type SystemPart struct {
	// Text is the prompt segment content.
	Text string

	// Static reports whether this segment belongs to the cache-eligible
	// prefix. Only segments before the first Static==false segment count;
	// a static segment that appears after a dynamic one does not receive
	// the cache_control marker.
	Static bool
}

// BuildCachedSystemPrompt converts parts into the Anthropic system array
// with an ephemeral cache_control breakpoint on the last block of the
// leading contiguous static prefix. Static blocks after the first
// dynamic block do not receive the marker, because doing so would bake
// the intervening dynamic text into the cached prefix and break cache
// hits on subsequent calls. Returns nil when parts is empty so the field
// omits cleanly via the wire struct's omitempty tag.
func BuildCachedSystemPrompt(parts []SystemPart) []ContentBlock {
	if len(parts) == 0 {
		return nil
	}

	prefixEnd := -1
	for i, p := range parts {
		if !p.Static {
			break
		}
		prefixEnd = i
	}

	blocks := make([]ContentBlock, len(parts))
	for i, p := range parts {
		blocks[i] = TextBlock(p.Text)
	}
	if prefixEnd >= 0 {
		blocks[prefixEnd].CacheControl = EphemeralCache()
	}
	return blocks
}

// BuildMessages returns a new slice that is history followed by a single
// user message containing newUser. Each history message's Content slice
// is copied into a fresh backing array so the helper never mutates the
// input slice, its elements, or their nested Content slices. Prefix
// stability is what lets the Anthropic prompt cache hit across calls
// that share the same history.
func BuildMessages(history []Message, newUser string) []Message {
	out := make([]Message, 0, len(history)+1)
	for _, msg := range history {
		msg.Content = append([]ContentBlock(nil), msg.Content...)
		out = append(out, msg)
	}
	out = append(out, Message{
		Role:    RoleUser,
		Content: []ContentBlock{TextBlock(newUser)},
	})
	return out
}
