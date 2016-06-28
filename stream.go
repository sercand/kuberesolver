package kuberesolver

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"google.golang.org/grpc/grpclog"
)

// Decoder allows StreamWatcher to watch any stream for which a Decoder can be written.
type decoder interface {
	// Decode should return the type of event, the decoded object, or an error.
	// An error will cause StreamWatcher to call Close(). Decode should block until
	// it has data or an error occurs.
	Decode() (object Event, err error)

	// Close should close the underlying io.Reader, signalling to the source of
	// the stream that it is no longer being watched. Close() must cause any
	// outstanding call to Decode() to return with an error of some sort.
	Close()
}

// Interface can be implemented by anything that knows how to watch and report changes.
type watchInterface interface {
	// Stops watching. Will close the channel returned by ResultChan(). Releases
	// any resources used by the watch.
	Stop()

	// Returns a chan which will receive all the events. If an error occurs
	// or Stop() is called, this channel will be closed, in which case the
	// watch should be completely cleaned up.
	ResultChan() <-chan Event
}

// JSONDecoder implements the Decoder interface for io.ReadClosers that
// have contents which consist of a series of watchEvent objects encoded via JSON.
type JSONDecoder struct {
	r       io.ReadCloser
	decoder *json.Decoder
}

// NewDecoder creates an Decoder for the given writer and codec.
func newDecoder(r io.ReadCloser) *JSONDecoder {
	return &JSONDecoder{
		r:       r,
		decoder: json.NewDecoder(r),
	}
}

// Decode blocks until it can return the next object in the writer. Returns an error
// if the writer is closed or an object can't be decoded.
func (d *JSONDecoder) Decode() (Event, error) {
	var got Event
	if err := d.decoder.Decode(&got); err != nil {
		return Event{}, err
	}
	switch got.Type {
	case Added, Modified, Deleted, Error:
		return got, nil
	default:
		return Event{}, fmt.Errorf("got invalid watch event type: %v", got.Type)
	}
}

// Close closes the underlying r.
func (d *JSONDecoder) Close() {
	grpclog.Printf("kuberesolver/stream.go: streamWatcher Close")
	d.r.Close()
}

// StreamWatcher turns any stream for which you can write a Decoder interface
// into a watch.Interface.
type streamWatcher struct {
	source decoder
	result chan Event
	sync.Mutex
	stopped bool
}

// NewStreamWatcher creates a StreamWatcher from the given io.ReadClosers.
func newStreamWatcher(r io.ReadCloser) watchInterface {
	sw := &streamWatcher{
		source: newDecoder(r),
		// It's easy for a consumer to add buffering via an extra
		// goroutine/channel, but impossible for them to remove it,
		// so nonbuffered is better.
		result: make(chan Event),
	}
	go sw.receive()
	return sw
}

// ResultChan implements Interface.
func (sw *streamWatcher) ResultChan() <-chan Event {
	return sw.result
}

// Stop implements Interface.
func (sw *streamWatcher) Stop() {
	// Call Close() exactly once by locking and setting a flag.
	grpclog.Printf("kuberesolver/stream.go: streamWatcher Stop")
	sw.Lock()
	defer sw.Unlock()
	if !sw.stopped {
		sw.stopped = true
		sw.source.Close()
	}
}

// stopping returns true if Stop() was called previously.
func (sw *streamWatcher) stopping() bool {
	sw.Lock()
	defer sw.Unlock()
	return sw.stopped
}

// receive reads result from the decoder in a loop and sends down the result channel.
func (sw *streamWatcher) receive() {
	defer close(sw.result)
	defer sw.Stop()
	for {
		obj, err := sw.source.Decode()
		if err != nil {
			// Ignore expected error.
			if sw.stopping() {
				return
			}
			switch err {
			case io.EOF:
				// watch closed normally
				grpclog.Printf("kuberesolver/stream.go: Watch closed normally")
			case io.ErrUnexpectedEOF:
				grpclog.Printf("kuberesolver/stream.go: Unexpected EOF during watch stream event decoding: %v", err)
			default:
				grpclog.Printf("kuberesolver/stream.go: Unable to decode an event from the watch stream: %v", err)
			}
			return
		}
		sw.result <- obj
	}
}
