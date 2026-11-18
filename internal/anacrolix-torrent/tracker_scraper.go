package torrent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/dht/v2/krpc"
	"github.com/anacrolix/log"

	"github.com/anacrolix/torrent/tracker"
)

// Announces a torrent to a tracker at regular intervals, when peers are
// required.
type trackerScraper struct {
	u               url.URL
	t               *Torrent
	lastAnnounce    trackerAnnounceResult
	lookupTrackerIp func(*url.URL) ([]net.IP, error)
	// Announces that failed in a row; only the Run goroutine touches it.
	consecutiveFailures int
	// What Run and a forced announce (announceWhenDue) share, so that neither starts while the
	// other is in flight and each waits out the other's result.
	ann announceState
}

// announceState is the one per-tracker record Run and announceWhenDue share. mu is a leaf lock:
// nothing is acquired while holding it, and it is never held across an announce.
type announceState struct {
	mu       sync.Mutex
	inFlight bool
	done     chan struct{} // closed when the announce in flight finishes
	// The completion of the last announce of either kind, and how long after it the next regular
	// announce may start.
	lastDone time.Time
	wait     time.Duration
	// Whether the last announce succeeded; only a success lets the wait be shortened or a
	// forced announce through.
	ok bool
	// The tracker's interval and min interval from its last successful response; 0 for a min
	// interval it did not send (a UDP tracker never does).
	interval, minInterval time.Duration
	// Whether a forced announce has gone out since Run's last one: at most one per regular
	// announce, or forced announces every min interval would keep pushing Run's out.
	forcedSinceRun bool
}

// reserveLocked marks an announce in flight; the caller holds mu and has seen none in flight.
func (a *announceState) reserveLocked() {
	a.inFlight = true
	a.done = make(chan struct{})
}

// releaseLocked ends the announce in flight and wakes whoever waits on it; the caller holds mu.
func (a *announceState) releaseLocked() {
	a.inFlight = false
	close(a.done)
}

// tryBegin reserves the tracker for Run's next announce when none is in flight and interval has
// passed since the last announce of either kind. Otherwise it returns false, with a channel that
// fires when the announce in flight ends, or nil when Run should recalculate at once: a forced
// announce moved the last announce, and the new wait must see wantPeers like any other.
func (me *trackerScraper) tryBegin(interval time.Duration) (retry <-chan struct{}, ok bool) {
	a := &me.ann
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inFlight {
		return a.done, false
	}
	if time.Until(a.lastDone.Add(interval)) > 0 {
		return nil, false
	}
	a.reserveLocked()
	return nil, true
}

// finishRun records Run's announce. raw is the response as the tracker sent it; wait and
// failures come from afterAnnounce.
func (me *trackerScraper) finishRun(raw trackerAnnounceResult, wait time.Duration) {
	a := &me.ann
	a.mu.Lock()
	defer a.mu.Unlock()
	a.record(raw, wait)
	a.forcedSinceRun = false
	a.releaseLocked()
}

// record stores an outcome; the caller holds mu.
func (a *announceState) record(raw trackerAnnounceResult, wait time.Duration) {
	a.lastDone = raw.Completed
	a.wait = wait
	a.ok = raw.Err == nil
	if a.ok {
		a.interval = raw.Interval
		a.minInterval = raw.MinInterval
	}
}

// nextWait returns what Run needs to schedule its next announce.
func (a *announceState) nextWait() (lastDone time.Time, wait time.Duration, shortenable bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastDone, a.wait, a.ok
}

// announceWhenDue sends one announce outside Run's schedule, when the tracker allows it and
// Run is not about to announce anyway: none in flight, the last announce of either kind
// succeeded, no forced one has gone out since Run's last, the tracker sent a min interval
// smaller than its interval, that min interval (never less than the client's announce floor)
// has passed since the last announce, and Run's own next announce is still ahead. It reports
// whether an announce went out. After a success Run waits the tracker's interval from this
// announce's completion. After a failure Run's next announce is due at the later of its previous
// due time and one min interval after the failure, so the tracker is neither hit sooner than
// its min interval nor left a full interval from the failure.
func (me *trackerScraper) announceWhenDue(ctx context.Context) bool {
	a := &me.ann
	floor := me.t.cl.config.announceFloor()
	a.mu.Lock()
	gap := a.minInterval
	if gap < floor {
		gap = floor
	}
	now := time.Now()
	if a.inFlight || a.forcedSinceRun || !a.ok || a.minInterval <= 0 || a.minInterval >= a.interval ||
		now.Before(a.lastDone.Add(gap)) || !now.Before(a.lastDone.Add(a.wait)) {
		a.mu.Unlock()
		return false
	}
	prevDue := a.lastDone.Add(a.wait)
	a.reserveLocked()
	a.forcedSinceRun = true
	a.mu.Unlock()

	ar := me.announce(ctx, tracker.None)

	a.mu.Lock()
	wait := a.wait
	if ar.Err == nil {
		wait = ar.Interval
		if wait < floor {
			wait = floor
		}
	} else {
		wait = prevDue.Sub(ar.Completed)
		if wait < gap {
			wait = gap
		}
	}
	a.record(ar, wait)
	a.releaseLocked()
	a.mu.Unlock()

	ar.Interval = wait
	me.t.cl.lock()
	me.lastAnnounce = ar
	me.t.cl.unlock()
	return true
}

// AnnounceWhenDue announces to each tracker of this torrent, and only those, that has sent a
// min interval shorter than its interval, whose min interval has passed since its last
// announce, and that Run is not already announcing to or about to. It adds no tracker, touches
// no DHT and no PEX, so it is safe for a private torrent. It returns the number of announces
// sent; 0 when none was due, so a caller can tell a deferral from an announce.
func (t *Torrent) AnnounceWhenDue() int {
	t.cl.rLock()
	scrapers := make([]*trackerScraper, 0, len(t.trackerAnnouncers))
	for _, ta := range t.trackerAnnouncers {
		if ts, ok := ta.(*trackerScraper); ok {
			scrapers = append(scrapers, ts)
		}
	}
	t.cl.rUnlock()

	var sent atomic.Int32
	var wg sync.WaitGroup
	for _, ts := range scrapers {
		wg.Add(1)
		go func(ts *trackerScraper) {
			defer wg.Done()
			if ts.announceWhenDue(t.closedCtx) {
				sent.Add(1)
			}
		}(ts)
	}
	wg.Wait()
	return int(sent.Load())
}

type torrentTrackerAnnouncer interface {
	statusLine() string
	URL() *url.URL
}

func (me *trackerScraper) URL() *url.URL {
	return &me.u
}

func (ts *trackerScraper) statusLine() string {
	var w bytes.Buffer
	fmt.Fprintf(&w, "next ann: %v, last ann: %v",
		func() string {
			na := time.Until(ts.lastAnnounce.Completed.Add(ts.lastAnnounce.Interval))
			if na > 0 {
				na /= time.Second
				na *= time.Second
				return na.String()
			} else {
				return "anytime"
			}
		}(),
		func() string {
			if ts.lastAnnounce.Err != nil {
				return ts.lastAnnounce.Err.Error()
			}
			if ts.lastAnnounce.Completed.IsZero() {
				return "never"
			}
			return fmt.Sprintf("%d peers", ts.lastAnnounce.NumPeers)
		}(),
	)
	return w.String()
}

type trackerAnnounceResult struct {
	Err      error
	NumPeers int
	Interval time.Duration
	// The tracker's min interval; 0 when it sent none.
	MinInterval time.Duration
	Completed   time.Time
}

func (me *trackerScraper) getIp() (ip net.IP, err error) {
	var ips []net.IP
	if me.lookupTrackerIp != nil {
		ips, err = me.lookupTrackerIp(&me.u)
	} else {
		// Do a regular dns lookup
		ips, err = net.LookupIP(me.u.Hostname())
	}
	if err != nil {
		return
	}
	if len(ips) == 0 {
		err = errors.New("no ips")
		return
	}
	me.t.cl.rLock()
	defer me.t.cl.rUnlock()
	if me.t.cl.closed.IsSet() {
		err = errors.New("client is closed")
		return
	}
	for _, ip = range ips {
		if me.t.cl.ipIsBlocked(ip) {
			continue
		}
		switch me.u.Scheme {
		case "udp4":
			if ip.To4() == nil {
				continue
			}
		case "udp6":
			if ip.To4() != nil {
				continue
			}
		}
		return
	}
	err = errors.New("no acceptable ips")
	return
}

func (me *trackerScraper) trackerUrl(ip net.IP) string {
	u := me.u
	if u.Port() != "" {
		u.Host = net.JoinHostPort(ip.String(), u.Port())
	}
	return u.String()
}

// Return how long to wait before trying again. For most errors, we return 5
// minutes, a relatively quick turn around for DNS changes.
func (me *trackerScraper) announce(ctx context.Context, event tracker.AnnounceEvent) (ret trackerAnnounceResult) {
	defer func() {
		ret.Completed = time.Now()
	}()
	ret.Interval = time.Minute

	// Limit concurrent use of the same tracker URL by the Client.
	ref := me.t.cl.activeAnnounceLimiter.GetRef(me.u.String())
	defer ref.Drop()
	select {
	case <-ctx.Done():
		ret.Err = ctx.Err()
		return
	case ref.C() <- struct{}{}:
	}
	defer func() {
		select {
		case <-ref.C():
		default:
			panic("should return immediately")
		}
	}()

	ip, err := me.getIp()
	if err != nil {
		ret.Err = fmt.Errorf("error getting ip: %s", err)
		return
	}
	me.t.cl.rLock()
	req := me.t.announceRequest(event)
	me.t.cl.rUnlock()
	// The default timeout works well as backpressure on concurrent access to the tracker. Since
	// we're passing our own Context now, we will include that timeout ourselves to maintain similar
	// behavior to previously, albeit with this context now being cancelled when the Torrent is
	// closed.
	ctx, cancel := context.WithTimeout(ctx, tracker.DefaultTrackerAnnounceTimeout)
	defer cancel()
	me.t.logger.WithDefaultLevel(log.Debug).Printf("announcing to %q: %#v", me.u.String(), req)
	res, err := tracker.Announce{
		Context:             ctx,
		HttpProxy:           me.t.cl.config.HTTPProxy,
		HttpRequestDirector: me.t.cl.config.HttpRequestDirector,
		DialContext:         me.t.cl.config.TrackerDialContext,
		ListenPacket:        me.t.cl.config.TrackerListenPacket,
		UserAgent:           me.t.cl.config.HTTPUserAgent,
		TrackerUrl:          me.trackerUrl(ip),
		Request:             req,
		HostHeader:          me.u.Host,
		ServerName:          me.u.Hostname(),
		UdpNetwork:          me.u.Scheme,
		ClientIp4:           krpc.NodeAddr{IP: me.t.cl.config.PublicIp4},
		ClientIp6:           krpc.NodeAddr{IP: me.t.cl.config.PublicIp6},
		Logger:              me.t.logger,
	}.Do()
	me.t.logger.WithDefaultLevel(log.Debug).Printf("announce to %q returned %#v: %v", me.u.String(), res, err)
	if err != nil {
		ret.Err = fmt.Errorf("announcing: %w", err)
		return
	}
	me.t.AddPeers(peerInfos(nil).AppendFromTracker(res.Peers))
	ret.NumPeers = len(res.Peers)
	ret.Interval = time.Duration(res.Interval) * time.Second
	ret.MinInterval = time.Duration(res.MinInterval) * time.Second
	return
}

// Returns whether we can shorten the interval, and sets notify to a channel that receives when we
// might change our mind, or leaves it if we won't.
func (me *trackerScraper) canIgnoreInterval(notify *<-chan struct{}) bool {
	gotInfo := me.t.GotInfo()
	select {
	case <-gotInfo:
		// Private trackers really don't like us announcing more than they specify. They're also
		// tracking us very carefully, so it's best to comply.
		private := me.t.info.Private
		return private == nil || !*private
	default:
		*notify = gotInfo
		return false
	}
}

// afterAnnounce records an announce outcome and returns the wait before the next one, and
// whether wanting peers may shorten it. A failure's wait never is: a dead tracker would be hit
// every minute by every torrent that lists it, exactly while the torrent is streaming.
func (me *trackerScraper) afterAnnounce(ar trackerAnnounceResult) (interval time.Duration, shortenable bool) {
	if ar.Err != nil {
		me.consecutiveFailures++
		return me.t.cl.config.failedAnnounceInterval(me.consecutiveFailures), false
	}
	me.consecutiveFailures = 0
	// Make sure we don't announce for at least a minute since the last one.
	interval = ar.Interval
	if floor := me.t.cl.config.announceFloor(); interval < floor {
		interval = floor
	}
	return interval, true
}

// retryDue reports whether a forced announce may go to this tracker now: not while a failed
// announce is still waiting out its backoff. Must be called with the client lock held.
func (me *trackerScraper) retryDue(now time.Time) bool {
	last := me.lastAnnounce
	return last.Err == nil || !now.Before(last.Completed.Add(last.Interval))
}

func (me *trackerScraper) Run() {
	defer me.announceStopped()

	// Backported from anacrolix/torrent upstream (commit 6e3fd9a9e5): use the torrent's shared
	// closedCtx instead of spawning an extra goroutine per tracker scraper just to bridge
	// me.t.Closed() into a locally-cancelable context. With torrents carrying many trackers,
	// that extra goroutine doubled the tracker-scraper goroutine count for no benefit.
	ctx := me.t.closedCtx

	// make sure first announce is a "started"
	e := tracker.Started

	// The first announce takes the reservation outright: a forced announce needs a successful
	// one before it may start.
	me.ann.mu.Lock()
	me.ann.reserveLocked()
	me.ann.mu.Unlock()

	for {
		ar := me.announce(ctx, e)
		// after first announce, get back to regular "none"
		e = tracker.None
		baseInterval, _ := me.afterAnnounce(ar)
		me.finishRun(ar, baseInterval)
		ar.Interval = baseInterval // statusLine reports the real next announce
		me.t.cl.lock()
		me.lastAnnounce = ar
		me.t.cl.unlock()

	recalculate:
		// The wait counts from the last announce of either kind: a forced one may have completed
		// since Run's own.
		lastDone, interval, shortenable := me.ann.nextWait()

		me.t.cl.lock()
		wantPeers := me.t.wantPeersEvent.C()
		me.t.cl.unlock()

		// If we want peers, reduce the interval to the minimum if it's appropriate.

		// A channel that receives when we should reconsider our interval. Starts as nil since that
		// never receives.
		var reconsider <-chan struct{}
		select {
		case <-wantPeers:
			if shortenable && interval > me.t.cl.config.announceFloor() && me.canIgnoreInterval(&reconsider) {
				interval = me.t.cl.config.announceFloor()
			}
		default:
			reconsider = wantPeers
		}

		select {
		case <-me.t.closed.Done():
			return
		case <-reconsider:
			// Recalculate the interval.
			goto recalculate
		case <-time.After(time.Until(lastDone.Add(interval))):
		}

		// Due. Start only when no forced announce is in flight and none has completed since
		// the wait was computed; otherwise wait again.
		retry, ok := me.tryBegin(interval)
		if !ok {
			if retry != nil {
				select {
				case <-me.t.closed.Done():
					return
				case <-retry:
				}
			}
			goto recalculate
		}
	}
}

func (me *trackerScraper) announceStopped() {
	ctx, cancel := context.WithTimeout(context.Background(), tracker.DefaultTrackerAnnounceTimeout)
	defer cancel()
	me.announce(ctx, tracker.Stopped)
}
