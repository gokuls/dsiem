package siem

import (
	"dsiem/internal/dsiem/pkg/event"
	"dsiem/internal/shared/pkg/idgen"
	log "dsiem/internal/shared/pkg/logger"
	"dsiem/internal/shared/pkg/str"
	"expvar"
	"strconv"
	"sync"
	"time"
)

type backlogs struct {
	sync.RWMutex
	id int
	bl map[string]*backLog
}

// allBacklogs doesnt need a lock, its size is fixed to the number
// of all loaded directives
var allBacklogs []backlogs

var backlogCounter = expvar.NewInt("backlog_counter")
var alarmCounter = expvar.NewInt("alarm_counter")

type removalChannelMsg struct {
	blogs *backlogs
	ID    string
}

var backLogRemovalChannel chan removalChannelMsg
var ticker *time.Ticker

var glock = &sync.RWMutex{}

// InitBackLog initialize backlog and ticker
func InitBackLog(logFile string) (err error) {
	bLogFile = logFile
	backLogRemovalChannel = make(chan removalChannelMsg)
	startWatchdogTicker()
	return
}

func removeBackLog(m removalChannelMsg) {
	m.blogs.Lock()
	defer m.blogs.Unlock()
	log.Debug(log.M{Msg: "Lock obtained. Removing backlog", BId: m.ID})
	glock.Lock()
	delete(m.blogs.bl, m.ID)
	glock.Unlock()
}

func updateAlarmCounter() (count int) {
	alarms.RLock()
	count = len(alarms.al)
	alarmCounter.Set(int64(count))
	alarms.RUnlock()
	return
}

// this checks for timed-out backlog and discard it
func startWatchdogTicker() {
	go func() {
		for {
			// handle incoming event, id should be the ID to remove
			msg := <-backLogRemovalChannel
			go removeBackLog(msg)
		}
	}()

	ticker = time.NewTicker(time.Second * 10)
	go func() {
		for {
			<-ticker.C
			log.Debug(log.M{Msg: "Watchdog tick started."})
			aLen := updateAlarmCounter()
			bLen := 0
			for i := range allBacklogs {
				glock.RLock()
				allBacklogs[i].RLock() // for length reading
				log.Debug(log.M{Msg: "ticker locked backlogs " + strconv.Itoa(allBacklogs[i].id)})
				l := len(allBacklogs[i].bl)
				if l == 0 {
					allBacklogs[i].RUnlock()
					glock.RUnlock()
					continue
				}
				bLen += l
				for j := range allBacklogs[i].bl {
					allBacklogs[i].bl[j].checkExpired()
				}
				allBacklogs[i].RUnlock()
				glock.RUnlock()
			}
			backlogCounter.Set(int64(bLen))
			eps := readEPS()
			log.Info(log.M{Msg: "Watchdog tick ended, # of backlogs:" + strconv.Itoa(bLen) +
				" alarms:" + strconv.Itoa(aLen) + eps})
		}
	}()
}

func readEPS() (res string) {
	if x := expvar.Get("eps_counter"); x != nil {
		res = " events/sec:" + x.String()
	}
	return
}

func (blogs *backlogs) manager(d *directive, ch <-chan event.NormalizedEvent) {
	for {
		// handle incoming event
		e := <-ch
		// first check existing backlog
		found := false
		// blogs.RLock()
		blogs.Lock() // test using writelock, to prevent double entry
		// log.Debug(log.M{Msg: "manager locked backlogs " + strconv.Itoa(blogs.id)})

		for _, v := range blogs.bl {
			v.RLock()
			cs := v.CurrentStage
			// only applicable for non-stage 1,
			// where there's more specific identifier like IP address to match
			// by convention, stage 1 rule *must* always have occurrence = 1
			if v.Directive.ID != d.ID || cs <= 1 {
				v.RUnlock()
				continue
			}
			// should check for currentStage rule match with event
			// heuristic, we know stage starts at 1 but rules start at 0
			idx := cs - 1
			currRule := v.Directive.Rules[idx]
			if !doesEventMatchRule(&e, &currRule, e.ConnID) {
				v.RUnlock()
				continue
			}
			log.Debug(log.M{Msg: " Event match with existing backlog. CurrentStage is " +
				strconv.Itoa(v.CurrentStage), DId: v.Directive.ID, BId: v.ID, CId: e.ConnID})
			v.RUnlock()
			found = true
			go blogs.bl[v.ID].processMatchedEvent(&e, idx)
			break
		}
		if found {
			// blogs.RUnlock()
			blogs.Unlock()
			continue // back to chan loop
		}

		// now for new backlog
		if !doesEventMatchRule(&e, &d.Rules[0], e.ConnID) {
			// blogs.RUnlock()
			blogs.Unlock()
			continue // back to chan loop
		}

		b, err := createNewBackLog(d, &e)
		if err != nil {
			log.Warn(log.M{Msg: "Fail to create new backlog", DId: d.ID, CId: e.ConnID})
			// blogs.RUnlock()
			blogs.Unlock()
			continue
		}
		glock.Lock()
		blogs.bl[b.ID] = &b
		blogs.bl[b.ID].bLogs = blogs
		blogs.Unlock()

		// should let first event processing finished before taking another event,
		// hence, not using go routine here
		blogs.bl[b.ID].processMatchedEvent(&e, 0)
		glock.Unlock()
	}
}

func createNewBackLog(d *directive, e *event.NormalizedEvent) (bp backLog, err error) {
	// create new backlog here, passing the event as the 1st event for the backlog
	bid, err := idgen.GenerateID()
	if err != nil {
		return
	}
	log.Info(log.M{Msg: "Creating new backlog", DId: d.ID, CId: e.ConnID})
	b := backLog{}
	b.ID = bid
	b.Directive = directive{}

	copyDirective(&b.Directive, d, e)
	initBackLogRules(&b.Directive, e)
	b.Directive.Rules[0].StartTime = time.Now().Unix()

	b.CurrentStage = 1
	b.HighestStage = len(d.Rules)
	bp = b
	return
}

func initBackLogRules(d *directive, e *event.NormalizedEvent) {
	for i := range d.Rules {
		// the first rule cannot use reference to other
		if i == 0 {
			d.Rules[i].Status = "active"
			continue
		}

		d.Rules[i].Status = "inactive"

		// for the rest, refer to the referenced stage if its not ANY or HOME_NET or !HOME_NET
		// if the reference is ANY || HOME_NET || !HOME_NET then refer to event if its in the format of
		// :ref

		r := d.Rules[i].From
		if v, ok := str.RefToDigit(r); ok {
			vmin1 := v - 1
			ref := d.Rules[vmin1].From
			if ref != "ANY" && ref != "HOME_NET" && ref != "!HOME_NET" {
				d.Rules[i].From = ref
			} else {
				d.Rules[i].From = e.SrcIP
			}
		}

		r = d.Rules[i].To
		if v, ok := str.RefToDigit(r); ok {
			vmin1 := v - 1
			ref := d.Rules[vmin1].To
			if ref != "ANY" && ref != "HOME_NET" && ref != "!HOME_NET" {
				d.Rules[i].To = ref
			} else {
				d.Rules[i].To = e.DstIP
			}
		}

		r = d.Rules[i].PortFrom
		if v, ok := str.RefToDigit(r); ok {
			vmin1 := v - 1
			ref := d.Rules[vmin1].PortFrom
			if ref != "ANY" {
				d.Rules[i].PortFrom = ref
			} else {
				d.Rules[i].PortFrom = strconv.Itoa(e.SrcPort)
			}
		}

		r = d.Rules[i].PortTo
		if v, ok := str.RefToDigit(r); ok {
			vmin1 := v - 1
			ref := d.Rules[vmin1].PortTo
			if ref != "ANY" {
				d.Rules[i].PortTo = ref
			} else {
				d.Rules[i].PortTo = strconv.Itoa(e.DstPort)
			}
		}

	}
}