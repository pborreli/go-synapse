package synapse

import (
	"github.com/blablacar/go-nerve/nerve"
	"github.com/n0rad/go-erlog/errs"
	"github.com/n0rad/go-erlog/logs"
	"github.com/samuel/go-zookeeper/zk"
	"strings"
	"sync"
	"time"
)

type WatcherZookeeper struct {
	WatcherCommon
	Hosts          []string
	Path           string
	TimeoutInMilli int

	reports          reportMap
	connection       *nerve.SharedZkConnection
	connectionEvents <-chan zk.Event
}

func NewWatcherZookeeper() *WatcherZookeeper {
	w := &WatcherZookeeper{
		TimeoutInMilli: 2000,
		reports:        NewNodes(),
	}
	return w
}

func (w *WatcherZookeeper) GetServiceName() string {
	return strings.Replace(w.Path, "/", "_", -1)[1:]
}

func (w *WatcherZookeeper) Init() error {
	if err := w.CommonInit(); err != nil {
		return errs.WithEF(err, w.fields, "Failed to init discovery")
	}
	w.fields = w.fields.WithField("path", w.Path)

	conn, err := nerve.NewSharedZkConnection(w.Hosts, time.Duration(w.TimeoutInMilli)*time.Millisecond)
	if err != nil {
		return errs.WithEF(err, w.fields, "Failed to prepare connection to zookeeper")
	}
	w.connection = conn
	w.connectionEvents = w.connection.Subscribe()
	return nil
}

func (w *WatcherZookeeper) Watch(stop <-chan struct{}, doneWaiter *sync.WaitGroup, events chan<- ServiceReport, s *Service) {
	doneWaiter.Add(1)
	defer doneWaiter.Done()
	watcherStop := make(chan struct{})
	watcherStopWaiter := sync.WaitGroup{}

	for {
		select {
		case <-w.reports.changed:
			reports := w.reports.getValues()
			logs.WithF(w.fields.WithField("data", reports)).Debug("Sending report")
			events <- ServiceReport{service: s, reports: reports}
		case e := <-w.connectionEvents:
			logs.WithF(w.fields.WithField("event", e)).Trace("Receiving event for connection")
			switch e.Type {
			case zk.EventSession | zk.EventType(0):
				if e.State == zk.StateHasSession {
					go w.watchRoot(watcherStop, &watcherStopWaiter)
				} else if e.State == zk.StateExpired || e.State == zk.StateDisconnected {
					close(watcherStop)
					watcherStopWaiter.Wait()

					watcherStop = make(chan struct{})
					watcherStopWaiter = sync.WaitGroup{}
				}
			}
		case <-stop:
			close(watcherStop)
			watcherStopWaiter.Wait()
			w.connection.Close()
			return
		}
	}
}

func (w *WatcherZookeeper) watchRoot(stop <-chan struct{}, doneWaiter *sync.WaitGroup) {
	doneWaiter.Add(1)
	defer doneWaiter.Done()

	for {
		exist, _, existEvent, err := w.connection.Conn.ExistsW(w.Path)
		if !exist {
			logs.WithF(w.fields).Warn("Path does not exists, waiting for creation")
			w.reports.setNoNodes()
			select {
			case <-existEvent:
			case <-stop:
				return
			}
		}

		childs, _, rootEvents, err := w.connection.Conn.ChildrenW(w.Path)
		if err != nil {
			logs.WithEF(err, w.fields.WithField("path", w.Path)).Warn("Cannot watch root service path")
		}

		if len(childs) == 0 {
			w.reports.setNoNodes()
		} else {
			for _, child := range childs {
				if _, ok := w.reports.get(child); !ok {
					go w.watchNode(w.Path+"/"+child, stop, doneWaiter)
				}
			}
		}

		select {
		case e := <-rootEvents:
			logs.WithF(w.fields.WithField("event", e)).Trace("Receiving event for root node")
			switch e.Type {
			case zk.EventNodeChildrenChanged | zk.EventNodeCreated | zk.EventNodeDataChanged | zk.EventNotWatching:
			// loop
			case zk.EventNodeDeleted:
				w.reports.removeAll()
			}
		case <-stop:
			return
		}
	}
}

func (w *WatcherZookeeper) watchNode(node string, stop <-chan struct{}, doneWaiter *sync.WaitGroup) {
	doneWaiter.Add(1)
	defer doneWaiter.Done()

	fields := w.fields.WithField("node", node)
	logs.WithF(fields).Debug("New node watcher")

	for {
		content, _, childEvent, err := w.connection.Conn.GetW(node)
		if err != nil {
			logs.WithEF(err, w.fields).Warn("Failed to watch node. Probably died just after arrival.")
			return
		}
		w.reports.addRawReport(node, content, fields)

		select {
		case e := <-childEvent:
			logs.WithF(fields.WithField("event", e)).Trace("Receiving event from node")
			switch e.Type {
			case zk.EventNodeDataChanged | zk.EventNodeCreated | zk.EventNotWatching:
			// loop
			case zk.EventNodeDeleted:
				w.reports.removeNode(node)
				return
			}
		case <-stop:
			return
		}

	}
}
