package message

import (
	"bytes"
	"strconv"
	"time"

	"github.com/saffat-in/trace/pkg/hash"
	"github.com/saffat-in/trace/pkg/log"
)

var zeroTime = time.Unix(0, 0)

// Topic types
const (
	TopicInvalid = uint8(iota)
	TopicStatic
	TopicWildcard
	TopicKeySeparator         = '/'
	TopicAnySeparator         = '*'
	TopicChildrenAllSeparator = "..."
	TopicSeparator            = '.'   // The separator character.
	MaxMessageSize            = 65536 // Maximum message size allowed from/to the peer.
)

// TopicOption represents a key/value pair option.
type TopicOption struct {
	Key   string
	Value string
}

// Topic represents a parsed topic.
type Topic struct {
	Key          []byte // Gets or sets the API key of the topic.
	Topic        []byte // Gets or sets the topic string.
	TopicOptions []byte
	Parts        []Part
	Depth        uint8
	Options      []TopicOption // Gets or sets the options.
	TopicType    uint8
}

type Part struct {
	Query     uint32
	Wildchars uint8
}

// SplitFunc various split function to split topic using delimeter
type splitFunc struct{}

func (splitFunc) splitKey(c rune) bool {
	return c == TopicKeySeparator
}

func (splitFunc) splitTopic(c rune) bool {
	return c == TopicSeparator
}

func (splitFunc) options(c rune) bool {
	return c == '?'
}

func (splitFunc) splitOptions(c rune) bool {
	return c == '&'
}
func (splitFunc) splitOpsKeyValue(c rune) bool {
	return c == '='
}

// Target returns the topic (first element of the query, second element of an SSID)
func (t *Topic) Target() uint32 {
	return t.Parts[0].Query
}

// TTL returns a Time-To-Live option.
func (t *Topic) TTL() (int64, bool) {
	ttl, sec, ok := t.getOption("ttl")
	if sec > 0 {
		return int64(time.Duration(sec) * time.Second), ok
	} else {
		var duration time.Duration
		duration, _ = time.ParseDuration(ttl)
		return int64(duration), ok
	}
}

// Last returns the 'last' option, which is a number of messages to retrieve.
func (t *Topic) Last() (time.Time, time.Time, int64, bool) {
	dur, last, ok := t.getOption("last")
	if ok {
		if last > 0 {
			u1 := time.Now().Unix() + 60
			return zeroTime, toUnix(u1), last, ok
		} else {
			base := time.Now()
			var duration time.Duration
			duration, _ = time.ParseDuration(dur)
			start := base.Add(-duration)
			return start, base, 0, ok
		}
	}

	return zeroTime, zeroTime, 0, ok
}

// Converts the time to Unix Time with validation.
func toUnix(t int64) time.Time {
	if t == 0 {
		return zeroTime
	}

	return time.Unix(t, 0)
}

// getOptUint retrieves a Uint option
func (t *Topic) getOption(name string) (string, int64, bool) {
	for i := 0; i < len(t.Options); i++ {
		if t.Options[i].Key == name {
			val, err := strconv.ParseInt(t.Options[i].Value, 10, 64)
			if err == nil {
				return "", int64(val), true
			}
			return t.Options[i].Value, 0, true
		}
	}
	return "", 0, false
}

// parseOptions parse the options from the topic
func (t *Topic) parseOptions(text []byte) (ok bool) {
	//Parse Options
	var fn splitFunc
	ops := bytes.FieldsFunc(text, fn.splitOptions)
	if ops != nil || len(ops) >= 1 {
		for _, o := range ops {
			op := bytes.FieldsFunc(o, fn.splitOpsKeyValue)
			if op == nil || len(op) < 2 {
				continue
			}
			t.Options = append(t.Options, TopicOption{
				Key:   unsafeToString(op[0]),
				Value: unsafeToString(op[1]),
			})
		}
	}
	return true
}

// ParseKey attempts to parse the key
func ParseKey(text []byte) (topic *Topic) {
	topic = new(Topic)
	var fn splitFunc

	parts := bytes.FieldsFunc(text, fn.splitKey)
	if parts == nil || len(parts) < 2 {
		topic.TopicType = TopicInvalid
		return topic
	}

	topic.Key = parts[0]

	parts = bytes.FieldsFunc(parts[1], fn.options)
	l := len(parts)
	if parts == nil || l < 1 {
		topic.TopicType = TopicInvalid
		return topic
	}
	if l > 1 {
		topic.TopicOptions = parts[1]
	}
	topic.Topic = parts[0]

	return topic
}

func (topic *Topic) Parse(contract uint32, wildcard bool) {
	if wildcard {
		parseWildcardTopic(contract, topic)
		return
	} else {
		parseStaticTopic(contract, topic)
	}

	return
}

// ParseTopic attempts to parse the topic from the underlying slice.
func parseStaticTopic(contract uint32, topic *Topic) (ok bool) {
	start := time.Now()
	defer log.ErrLogger.Debug().Str("context", "topic.parseStaticTopic").Dur("duration", time.Since(start)).Msg("")

	var part Part
	var fn splitFunc
	topic.Parts = make([]Part, 0, 6)
	log.Debug("topic.parseStaticTopic", "topic name "+string(topic.Topic))
	ok = topic.parseOptions(topic.TopicOptions)

	if !ok {
		topic.TopicType = TopicInvalid
		return false
	}

	parts := bytes.FieldsFunc(topic.Topic, fn.splitTopic)
	part = Part{}
	for _, p := range parts {
		part.Query = hash.WithSalt(p, contract)
		topic.Parts = append(topic.Parts, part)
	}

	topic.Depth = uint8(len(topic.Parts))
	topic.TopicType = TopicStatic
	return true
}

// ParseTopic attempts to parse the topic from the underlying slice.
func parseWildcardTopic(contract uint32, topic *Topic) (ok bool) {
	start := time.Now()
	defer log.ErrLogger.Debug().Str("context", "topic.parseWildcardTopic").Dur("duration", time.Since(start)).Msg("")

	var part Part
	var fn splitFunc
	topic.Parts = make([]Part, 0, 6)
	log.Debug("topic.parseWildcardTopic", "topic name "+string(topic.Topic))
	ok = topic.parseOptions(topic.TopicOptions)

	if !ok {
		topic.TopicType = TopicInvalid
		return false
	}

	depth := uint8(0)
	q := []byte(TopicChildrenAllSeparator)
	if bytes.HasSuffix(topic.Topic, q) {
		topic.Topic = bytes.TrimRight(topic.Topic, string(TopicChildrenAllSeparator))
		topic.TopicType = TopicWildcard
		topic.Depth = 23

		if len(topic.Topic) == 0 {
			part.Query = wildcard
			topic.Parts = append(topic.Parts, part)
			return false
		}
	}

	parts := bytes.FieldsFunc(topic.Topic, fn.splitTopic)
	q = []byte{TopicAnySeparator}
	part = Part{}
	wildchars := uint8(0)
	wildcharcount := 0
	for idx, p := range parts {
		depth++
		if bytes.HasSuffix(p, q) {
			topic.TopicType = TopicWildcard
			if idx == 0 {
				part.Query = hash.WithSalt(p, contract)
				topic.Parts = append(topic.Parts, part)
			}
			wildchars++
			wildcharcount++
			continue
		}
		part.Query = hash.WithSalt(p, contract)
		topic.Parts = append(topic.Parts, part)
		if wildchars > 0 {
			if idx-wildcharcount-1 >= 0 {
				topic.Parts[idx-wildcharcount-1].Wildchars = wildchars
			} else {
				topic.Parts[0].Wildchars = wildchars
			}
			wildchars = 0
		}
	}

	if wildchars > 0 {
		topic.Parts[len(topic.Parts)-1:][0].Wildchars = wildchars
	}
	topic.Depth += depth

	if topic.TopicType != TopicWildcard {
		topic.TopicType = TopicStatic
	}
	return true
}
