package nerve

import (
	"encoding/json"
	"github.com/n0rad/go-erlog/data"
	"github.com/n0rad/go-erlog/errs"
	"github.com/n0rad/go-erlog/logs"
	"math"
	"strconv"
	"sync"
	"time"
)

type Service struct {
	Name                                 string
	Port                                 int
	Host                                 string
	PreferIpv4                           bool
	Weight                               uint8
	Checks                               []json.RawMessage
	Reporters                            []json.RawMessage
	ReportReplayInMilli                  int
	HaproxyServerOptions                 string
	SetServiceAsDownOnShutdown           *bool
	Labels                               map[string]string
	EnableCheckStableCommand             []string
	EnableWarmupIntervalInMilli          int
	EnableWarmupMaxDurationInMilli       int
	DisableGracefullyDoneCommand         []string
	DisableGracefullyDoneIntervalInMilli int
	DisableMaxDurationInMilli            int
	DisableMinDurationInMilli            int
	NoMetrics                            bool

	nerve                      *Nerve
	disabled                   error
	disableMutex               sync.Mutex
	warmupGiveUp               chan struct{}
	warmupMutex                sync.Mutex
	warmupGiveUpMutex          sync.Mutex
	currentWeightIndex         int
	currentStatus              *error
	typedCheckersWithStatus    map[Checker]*error
	typedReportersWithReported map[Reporter]bool
	fields                     data.Fields
}

var weights = []float64{0, 1, 2, 3, 5, 8, 13, 21, 34, 55, 89, 144, 233}

const postFullWeightMax = 10

func (s *Service) Init(n *Nerve) error {
	logs.WithField("data", s).Info("service loaded") // todo rewrite with conf only
	s.nerve = n

	if s.Host == "" {
		s.Host = "127.0.0.1"
	}
	if s.Name == "" {
		s.Name = s.Host + ":" + strconv.Itoa(s.Port)
	}

	if s.SetServiceAsDownOnShutdown == nil {
		val := true
		s.SetServiceAsDownOnShutdown = &val
	}

	if s.Weight == 0 {
		s.Weight = 255
	}
	if s.ReportReplayInMilli == 0 {
		s.ReportReplayInMilli = 1000
	}
	if s.EnableWarmupIntervalInMilli == 0 {
		s.EnableWarmupIntervalInMilli = 1000
	}
	if s.EnableWarmupMaxDurationInMilli == 0 {
		s.EnableWarmupMaxDurationInMilli = 2 * 60 * 1000
	}

	if s.DisableGracefullyDoneIntervalInMilli == 0 {
		s.DisableGracefullyDoneIntervalInMilli = 1000
	}
	if s.DisableMinDurationInMilli == 0 {
		s.DisableMinDurationInMilli = 3000
	}
	if s.DisableMaxDurationInMilli == 0 {
		s.DisableMaxDurationInMilli = 60 * 1000
	}

	s.typedReportersWithReported = make(map[Reporter]bool)
	s.typedCheckersWithStatus = make(map[Checker]*error)

	s.fields = data.WithField("service", s.Host+":"+strconv.Itoa(s.Port))
	for _, data := range s.Checks {
		checker, err := CheckerFromJson(data, s)
		if err != nil {
			return errs.WithEF(err, s.fields, "Failed to load check")
		}
		logs.WithF(s.fields).WithFields(checker.GetFields()).Info("check loaded")
		s.typedCheckersWithStatus[checker] = nil
	}
	if len(s.typedCheckersWithStatus) == 0 {
		logs.WithF(s.fields).Warn("No check specified, adding tcp")
		checker := NewCheckTcp()
		checker.Type = "tcp"
		checker.Init(s)
		s.typedCheckersWithStatus[checker] = nil
	}

	for _, data := range s.Reporters {
		reporter, err := ReporterFromJson(data, s)
		if err != nil {
			return errs.WithEF(err, s.fields, "Failed to load reporter")
		}
		logs.WithF(s.fields).WithFields(reporter.GetFields()).Info("Reporter loaded")
		s.typedReportersWithReported[reporter] = true
	}
	if len(s.typedReportersWithReported) == 0 {
		logs.WithF(s.fields).Warn("No reporter specified, adding console")
		s.typedReportersWithReported[NewReporterConsole()] = true
	}

	return nil
}

func (s *Service) Start(stopper <-chan struct{}, stopWait *sync.WaitGroup) {
	logs.WithFields(s.fields).Info("Starting service check")
	stopWait.Add(1)
	defer stopWait.Done()
	checkStopWait := &sync.WaitGroup{}

	statusChange := make(chan Check)
	for checker := range s.typedCheckersWithStatus {
		go checker.Run(statusChange, stopper, checkStopWait)
	}

	for {
		select {
		case status := <-statusChange:
			logs.WithF(s.fields.WithField("status", status)).Debug("New status received")
			s.processStatus(status)
		case <-stopper:
			logs.WithFields(s.fields).Debug("Stop requested")
			checkStopWait.Wait()
			close(statusChange)
			if *s.SetServiceAsDownOnShutdown {
				wait := &sync.WaitGroup{}
				wait.Add(1)
				s.Disable(wait)
				wait.Wait()
			}
			for reporter := range s.typedReportersWithReported {
				reporter.Destroy()
			}
			return
		case <-time.After(time.Duration(s.ReportReplayInMilli) * time.Millisecond):
			s.reportAndTellIfAtLeastOneReported(false)
		}
	}
}

func (s *Service) processStatus(check Check) {
	s.typedCheckersWithStatus[check.Checker] = &check.Status
	var combinedStatus error
	for _, status := range s.typedCheckersWithStatus {
		if status == nil {
			logs.WithF(s.fields).Debug("One check have no value, cannot report yet")
			return
		}
		if combinedStatus == nil {
			combinedStatus = *status
		}
	}

	if logs.IsDebugEnabled() {
		logs.WithF(s.fields.WithField("status", check).WithField("combined", combinedStatus)).Debug("combined status process")
	}

	if s.currentStatus == nil ||
		(*s.currentStatus == nil && combinedStatus != nil) ||
		(*s.currentStatus != nil && combinedStatus == nil) {
		s.currentStatus = &combinedStatus

		s.giveUpWarmup()

		if combinedStatus == nil {
			logs.WithF(s.fields).Info("Service is available")
			s.warmup()
		} else {
			if !s.NoMetrics {
				s.nerve.availableGauge.WithLabelValues(s.Name).Set(0)
			}
			s.currentWeightIndex = 0
			logs.WithEF(combinedStatus, s.fields).Warn("Service is not available")
			s.reportAndTellIfAtLeastOneReported(true)
		}

	} else {
		logs.WithF(s.fields).Debug("Combined status is same as previous, no report required")
	}
}

func (s *Service) giveUpWarmup() {
	s.warmupGiveUpMutex.Lock()
	defer s.warmupGiveUpMutex.Unlock()

	if s.warmupGiveUp != nil {
		close(s.warmupGiveUp)
		s.warmupGiveUp = nil
	}
}

func (s *Service) warmup() {
	s.warmupMutex.Lock()
	defer s.warmupMutex.Unlock()

	s.giveUpWarmup()
	s.warmupGiveUp = make(chan struct{})
	go s.Warmup(s.warmupGiveUp)
}

func (s *Service) Warmup(giveUp <-chan struct{}) {
	start := time.Now()
	s.currentWeightIndex = 0
	s.reportAndTellIfAtLeastOneReported(true)
	for {
		if len(s.EnableCheckStableCommand) > 0 {
			if err := ExecCommand(s.EnableCheckStableCommand, s.EnableWarmupIntervalInMilli); err != nil {
				logs.WithEF(err, s.fields).Warn("Check stable command failed. Reset weight")
				s.currentWeightIndex = 0
			} else {
				s.currentWeightIndex++
			}
		} else {
			s.currentWeightIndex++
		}

		if !s.NoMetrics {
			s.nerve.availableGauge.WithLabelValues(s.Name).Set(float64(s.CurrentWeight()))
		}

		if s.currentWeightIndex < len(weights) && !s.reportAndTellIfAtLeastOneReported(true) {
			logs.WithF(s.fields).Debug("No report succeed. Reset weight")
			s.currentWeightIndex = 0
		}

		if s.currentWeightIndex > postFullWeightMax+len(weights) {
			logs.WithF(s.fields).Debug("Service is fully stable")
			s.warmupMutex.Lock()
			defer s.warmupMutex.Unlock()
			s.warmupGiveUp = nil
			return
		}

		if time.Now().After(start.Add(time.Duration(s.EnableWarmupMaxDurationInMilli) * time.Millisecond)) {
			logs.WithF(s.fields).Warn("Warmup reach max duration. set Full Weight")
			s.currentWeightIndex = len(weights) - 1
			s.reportAndTellIfAtLeastOneReported(true)
			return
		}

		select {
		case <-giveUp:
			logs.WithF(s.fields).Debug("Warmup giveup requested")
			return
		case <-time.After(time.Duration(s.EnableWarmupIntervalInMilli) * time.Millisecond):
		}
	}

}

func (s *Service) reportAndTellIfAtLeastOneReported(required bool) bool {
	if s.currentStatus == nil {
		return false // no status yet
	}
	status := *s.currentStatus
	if s.disabled != nil {
		status = s.disabled
	}
	report := toReport(status, s)
	globalReported := 0
	for reporter, reported := range s.typedReportersWithReported {
		if required || !reported {
			logs.WithFields(s.fields).WithField("reporter", reporter).WithField("report", report).Debug("Sending report")
			if err := reporter.Report(report); err != nil {
				logs.WithEF(err, s.fields.WithFields(reporter.GetFields())).Error("Failed to report")
				if !s.NoMetrics {
					s.nerve.reporterFailureCount.WithLabelValues(s.Name, reporter.getCommon().Type).Inc()
				}
				s.typedReportersWithReported[reporter] = false
			} else {
				s.typedReportersWithReported[reporter] = true
				globalReported++
			}
		}
	}
	return globalReported > 0
}

func (s *Service) CurrentWeight() uint8 {
	if s.currentStatus == nil || *s.currentStatus != nil {
		return 0
	}

	index := s.currentWeightIndex
	if s.currentWeightIndex > len(weights)-1 {
		index = len(weights) - 1
	}
	res := uint8(math.Ceil(weights[index] * float64(s.Weight) / weights[len(weights)-1]))
	if res == 0 {
		res++
	}
	return res
}

func (s *Service) Disable(doneWaiter *sync.WaitGroup) {
	defer doneWaiter.Done()
	s.disableMutex.Lock()
	defer s.disableMutex.Unlock()

	s.giveUpWarmup()

	start := time.Now()
	s.disabled = errs.With("Service is disabled")
	s.reportAndTellIfAtLeastOneReported(true)

	if len(s.DisableGracefullyDoneCommand) > 0 {
		for {
			var err error
			if err = ExecCommand(s.DisableGracefullyDoneCommand, s.DisableGracefullyDoneIntervalInMilli); err == nil {
				logs.WithF(s.fields).Debug("Gracefull check succeed")
				break
			}
			logs.WithEF(err, s.fields).Debug("Gracefull check command fail")

			select {
			case <-time.After(start.Add(time.Duration(s.DisableMaxDurationInMilli) * time.Millisecond).Sub(time.Now())):
				logs.WithF(s.fields).Debug("Disable max duration reached")
				return
			case <-time.After(time.Duration(s.DisableGracefullyDoneIntervalInMilli) * time.Millisecond):
			}
		}
	}

	time.Sleep(start.Add(time.Duration(s.DisableMinDurationInMilli) * time.Millisecond).Sub(time.Now()))
}

func (s *Service) Enable() {
	s.disableMutex.Lock()
	defer s.disableMutex.Unlock()
	s.disabled = nil
	if s.warmupGiveUp == nil {
		s.warmup()
	}
}