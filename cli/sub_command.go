// Copyright 2020-2025 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/choria-io/fisk"
	"github.com/dustin/go-humanize"
	"github.com/nats-io/jsm.go"
	"github.com/nats-io/jsm.go/api"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/natscli/internal/asciigraph"
	iu "github.com/nats-io/natscli/internal/util"
	"github.com/synadia-io/orbit.go/jetstreamext"
	terminal "golang.org/x/term"
)

type subCmd struct {
	subjects              []string
	queue                 string
	durable               string
	raw                   bool
	translate             string
	jsAck                 bool
	inbox                 bool
	match                 bool
	dump                  string
	limit                 uint
	sseq                  uint64
	deliverAll            bool
	deliverNew            bool
	reportSubjects        bool
	reportSubjectsCount   int
	reportSub             bool
	deliverLast           bool
	deliverSince          string
	deliverLastPerSubject bool
	headersOnly           bool
	stream                string
	jetStream             bool
	ignoreSubjects        []string
	wait                  time.Duration
	timeStamps            bool
	deltaTimeStamps       bool
	subjectsOnly          bool
	graphOnly             bool
	width                 int
	height                int
	messageRates          map[string]*subMessageRate
	direct                bool
	streamObj             *jsm.Stream
}

type subMessageRate struct {
	lastCount int
	rates     []float64
}

func configureSubCommand(app commandHost) {
	c := &subCmd{}

	subHelp := `
	Jetstream will be activated when related options like --stream, --durable or --ack are supplied.

		E.g. nats sub <subject that is bound to a stream> --all

	Currently only supports push subscriptions. Uses an ephemeral consumer without ack by default.  

	For specific consumer options please pre-create a consumer using 'nats consumer add'.app.

		E.g. when explicit acknowledgement is required.

	Caution: Be careful when subscribing to streams with WorkQueue policy. Messages will be acked and deleted when a durable consumer is being used.

	Use nats stream view <stream> for inspecting messages.	
		
	`

	act := app.Command("subscribe", "Generic subscription client").Alias("sub").Action(c.subscribe)
	act.HelpLong(subHelp)
	addCheat("sub", act)

	act.Arg("subjects", "Subjects to subscribe to").StringsVar(&c.subjects)
	act.Flag("queue", "Subscribe to a named queue group").StringVar(&c.queue)
	act.Flag("durable", "Use a durable consumer (requires JetStream)").StringVar(&c.durable)
	act.Flag("raw", "Show the raw data received").Short('r').UnNegatableBoolVar(&c.raw)
	act.Flag("translate", "Translate the message data by running it through the given command before output").StringVar(&c.translate)
	act.Flag("ack", "Acknowledge JetStream message that have the correct metadata").BoolVar(&c.jsAck)
	// We do not support (explicit) ackPolicy right now. The only situation where it is useful would be WorkQueue policy right now.
	// Deleting from a stream with WorkQueue through ack could be unexpected behavior in the sub command.
	// To be done - check for streams with WorkQueue, then prompt with or allow with override --force=WorkQueueDelete
	// act.Flag("ackPolicy", "Acknowledgment policy (none, all, explicit) (requires JetStream)").Default("none").EnumVar(&c.ackPolicy, "none", "all", "explicit")
	act.Flag("match-replies", "Match replies to requests").UnNegatableBoolVar(&c.match)
	act.Flag("inbox", "Subscribes to a generate inbox").Short('i').UnNegatableBoolVar(&c.inbox)
	act.Flag("count", "Quit after receiving this many messages").UintVar(&c.limit)
	act.Flag("dump", "Dump received messages to files, 1 file per message. Specify - for null terminated STDOUT for use with xargs -0").PlaceHolder("DIRECTORY").StringVar(&c.dump)
	act.Flag("headers-only", "Do not render any data, shows only headers").UnNegatableBoolVar(&c.headersOnly)
	act.Flag("subjects-only", "Prints only the messages' subjects").UnNegatableBoolVar(&c.subjectsOnly)
	act.Flag("start-sequence", "Starts at a specific Stream sequence (requires JetStream)").PlaceHolder("SEQUENCE").Uint64Var(&c.sseq)
	act.Flag("all", "Delivers all messages found in the Stream (requires JetStream)").UnNegatableBoolVar(&c.deliverAll)
	act.Flag("new", "Delivers only future messages (requires JetStream)").UnNegatableBoolVar(&c.deliverNew)
	act.Flag("last", "Delivers the most recent and all future messages (requires JetStream)").UnNegatableBoolVar(&c.deliverLast)
	act.Flag("since", "Delivers messages received since a duration like 1d3h5m2s(requires JetStream)").PlaceHolder("DURATION").StringVar(&c.deliverSince)
	act.Flag("last-per-subject", "Deliver the most recent messages for each subject in the Stream (requires JetStream)").UnNegatableBoolVar(&c.deliverLastPerSubject)
	act.Flag("stream", "Subscribe to a specific stream (required JetStream)").PlaceHolder("STREAM").StringVar(&c.stream)
	act.Flag("ignore-subject", "Subjects for which corresponding messages will be ignored and therefore not shown in the output").Short('I').PlaceHolder("SUBJECT").StringsVar(&c.ignoreSubjects)
	act.Flag("wait", "Unsubscribe after this amount of time without any traffic").DurationVar(&c.wait)
	act.Flag("report-subjects", "Subscribes to subject patterns and builds a de-duplicated report of active subjects receiving data").UnNegatableBoolVar(&c.reportSubjects)
	act.Flag("report-subscriptions", "Subscribes to subject patterns and builds a de-duplicated report of active subscriptions receiving data").UnNegatableBoolVar(&c.reportSub)
	act.Flag("report-top", "Number of subjects to show when doing 'report-subjects'. Default is 10.").Default("10").IntVar(&c.reportSubjectsCount)
	act.Flag("timestamp", "Show timestamps in output").Short('t').UnNegatableBoolVar(&c.timeStamps)
	act.Flag("delta-time", "Show time since start in output").Short('d').UnNegatableBoolVar(&c.deltaTimeStamps)
	act.Flag("graph", "Graph the rate of messages received").UnNegatableBoolVar(&c.graphOnly)
	act.Flag("direct", "Subscribe using batched direct gets instead of a durable consumer").UnNegatableBoolVar(&c.direct)
}

func init() {
	registerCommand("sub", 17, configureSubCommand)
}

func (c *subCmd) startGraph(ctx context.Context, mu *sync.Mutex) {
	resizeData := func(data []float64, width int) []float64 {
		if width <= 0 {
			return data
		}

		length := len(data)

		if length > width {
			return data[length-width:]
		}

		return data
	}

	go func() {
		ticker := time.NewTicker(time.Second)

		for {
			select {
			case <-ticker.C:
				mu.Lock()

				width, height, err := terminal.GetSize(int(os.Stdout.Fd()))
				if err != nil {
					width = c.width
					height = c.height
				}
				c.width = width
				c.height = height
				if c.width > 15 {
					c.width -= 10
				}
				if c.height > 10 {
					c.height -= 6
				}

				iu.ClearScreen()

				for _, subject := range c.subjects {
					rates := c.messageRates[subject]
					count := 0
					if rates.lastCount > 0 {
						count = rates.lastCount
						rates.lastCount = 0
					}
					rates.rates = append(rates.rates, float64(count))
					rates.rates = resizeData(rates.rates, c.width)

					msgRatePlot := asciigraph.Plot(rates.rates,
						asciigraph.Caption(fmt.Sprintf("%s / second", subject)),
						asciigraph.Width(c.width),
						asciigraph.Height((c.height/(len(c.subjects)+1))-1),
						asciigraph.LowerBound(0),
						asciigraph.Precision(0),
						asciigraph.ValueFormatter(f),
					)
					fmt.Println(msgRatePlot)
					fmt.Println()
				}

				mu.Unlock()
			case <-ctx.Done():
				ticker.Stop()
			}
		}
	}()
}

func (c *subCmd) startSubjectReporting(ctx context.Context, subjMu *sync.Mutex, subjectReportMap map[string]int64, subjectBytesReportMap map[string]int64, subjCount int) {
	go func() {
		ticker := time.NewTicker(time.Second)

		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
			case <-ticker.C:
				subjectRows := [][]any{}

				if runtime.GOOS != "windows" {
					fmt.Print("\033[2J")
					fmt.Print("\033[H")
				}

				totalBytes := int64(0)
				totalCount := int64(0)

				subjMu.Lock()
				keys := make([]string, 0, len(subjectReportMap))

				for k := range subjectReportMap {
					keys = append(keys, k)
				}
				// sort.Strings(keys)
				// sort by count in descending order, and if count is equal, by
				// subject in ascending order.
				sort.Slice(keys, func(i, j int) bool {
					lhs, rhs := subjectReportMap[keys[i]], subjectReportMap[keys[j]]
					if lhs == rhs {
						return keys[i] < keys[j]
					}
					return lhs > rhs
				})

				for count, k := range keys {

					subjectRows = append(subjectRows, []any{k, f(subjectReportMap[k]), humanize.IBytes(uint64(subjectBytesReportMap[k]))})
					totalCount += subjectReportMap[k]
					totalBytes += subjectBytesReportMap[k]
					if (count + 1) == subjCount {
						break
					}
				}
				subjMu.Unlock()

				tableHeaderString := ""
				if subjCount == 1 {
					tableHeaderString = "Top Subject Report"
				} else {
					tableHeaderString = fmt.Sprintf("Top %d Active Subjects Report", subjCount)
				}
				table := iu.NewTableWriter(opts(), tableHeaderString)
				table.AddHeaders("Subject", "Message Count", "Bytes")
				table.AddFooter("Totals", f(totalCount), humanize.IBytes(uint64(totalBytes)))
				for i := range subjectRows {
					table.AddRow(subjectRows[i]...)
				}
				fmt.Println(table.Render())
			}
		}
	}()
}

func (c *subCmd) validateInputs(nc *nats.Conn, mgr *jsm.Manager) error {
	c.jetStream = c.sseq > 0 || len(c.durable) > 0 || c.deliverAll || c.deliverNew || c.deliverLast || c.deliverSince != "" || c.deliverLastPerSubject || c.stream != "" || c.direct

	switch {
	case len(c.subjects) == 0 && c.inbox:
		c.subjects = []string{nc.NewRespInbox()}
	case len(c.subjects) == 0 && c.stream == "":
		return fmt.Errorf("subject is required")
	case len(c.subjects) > 1 && c.jetStream:
		return fmt.Errorf("streams subscribe support only 1 subject")
	}

	if c.inbox && c.jetStream {
		return fmt.Errorf("generating inboxes is not compatible with JetStream subscriptions")
	}
	if c.queue != "" && c.jetStream {
		return fmt.Errorf("queue group subscriptions are not supported with JetStream")
	}
	if c.dump == "-" && c.inbox {
		return fmt.Errorf("generating inboxes is not compatible with dumping to stdout using null terminated strings")
	}
	if c.reportSubjects && c.reportSubjectsCount == 0 {
		return fmt.Errorf("subject count must be at least one")
	}
	if c.reportSub {
		c.reportSubjects = true
	}

	if c.timeStamps && c.deltaTimeStamps {
		return fmt.Errorf("timestamp and delta-time flags are mutually exclusive")
	}

	if c.dump != "" && c.dump != "-" {
		err := os.MkdirAll(c.dump, 0700)
		if err != nil {
			return err
		}
	}

	if c.jetStream {
		// if we are passed subjects without a stream, we determine the stream name before trying to load it for further checks
		if c.stream == "" {
			if len(c.subjects) > 1 {
				return fmt.Errorf("cannot enable direct gets: cannot perform direct gets from multiple subjects")

			}
			streams, err := mgr.StreamNames(&jsm.StreamNamesFilter{
				Subject: c.subjects[0],
			})
			if err != nil {
				return err
			}

			if len(streams) > 1 {
				return fmt.Errorf("cannot enable direct gets: subject mapped to multiple streams")
			} else if len(streams) == 0 {
				return fmt.Errorf("cannot enable direct gets: subject mapped to no streams")
			}

			c.stream = streams[0]
		}

		var err error
		c.streamObj, err = mgr.LoadStream(c.stream)
		if err != nil {
			return err
		}

		// Check if the stream is a Work Queue
		c.direct = c.direct || c.streamObj.Retention() == api.RetentionPolicy(nats.WorkQueuePolicy)

		if c.direct {
			// Direct Addressing needs to be allowed on the stream
			if !c.streamObj.DirectAllowed() {
				return fmt.Errorf("cannot enable direct gets: allow_direct is not set to true for stream %s", c.stream)
			}
			err := iu.RequireAPILevel(mgr, 1, "subscribing using direct get requires NATS Server 2.11")
			if err != nil {
				return err
			}
		}
	}

	return nil
}

type msgHandlerState struct {
	msgMu     *sync.Mutex
	cancelFn  context.CancelFunc
	counter   *uint
	startTime time.Time
}

func (c *subCmd) createMsgHandler(hcfg msgHandlerState, nc *nats.Conn, subjMu *sync.Mutex, subs *[]*nats.Subscription, ignoreSubjects []string, subjectReportMap map[string]int64, subjectBytesReportMap map[string]int64, matchMap map[string]*nats.Msg, t *time.Timer, dump bool) nats.MsgHandler {
	return func(m *nats.Msg) {
		hcfg.msgMu.Lock()
		defer hcfg.msgMu.Unlock()

		if c.shouldIgnore(m, ignoreSubjects) {
			return
		}

		if c.handleFlowControlMsg(m, nc) {
			return
		}

		if c.shouldAck(m) {
			defer func() {
				err := m.Respond(nil)
				if err != nil && !dump && !c.raw {
					log.Printf("Acknowledging message via subject %s failed: %s\n", m.Reply, err)
				}
			}()
		}

		*hcfg.counter++

		switch {
		case c.reportSubjects:
			c.handleSubjectReport(m, subjMu, subjectReportMap, subjectBytesReportMap)
		case c.graphOnly:
			c.handleGraphUpdate(m, subjMu)
		default:
			c.handleMsg(m, matchMap, hcfg)
		}

		if c.limit > 0 && *hcfg.counter == c.limit {
			for _, sub := range *subs {
				sub.Unsubscribe()
			}

			// if no reply matching, or if didn't yet get all replies
			if !c.match || len(matchMap) == 0 {
				hcfg.cancelFn()
			}
			return
		}

		if t != nil && !t.Reset(c.wait) {
			hcfg.cancelFn()
			return
		}
	}
}

func (c *subCmd) shouldIgnore(m *nats.Msg, ignoreSubjects []string) bool {
	for _, ignore := range ignoreSubjects {
		if server.SubjectsCollide(m.Subject, ignore) {
			return true
		}
	}
	return false
}

func (c *subCmd) handleFlowControlMsg(m *nats.Msg, nc *nats.Conn) bool {
	if c.jetStream && len(m.Data) == 0 && m.Header.Get("Status") == "100" {
		if m.Reply != "" {
			_ = m.Respond(nil)
			log.Printf("Responding to Flow Control message")
		} else if stalled := m.Header.Get("Nats-Consumer-Stalled"); stalled != "" {
			_ = nc.Publish(stalled, nil)
			log.Printf("Resuming stalled consumer")
		}
		return true
	}
	return false
}

func (c *subCmd) shouldAck(m *nats.Msg) bool {
	var info *jsm.MsgInfo
	if m.Reply != "" {
		info, _ = jsm.ParseJSMsgMetadata(m)
	}
	if c.jsAck && info != nil {
		return true
	}
	return false
}

func (c *subCmd) handleSubjectReport(m *nats.Msg, mu *sync.Mutex, reportMap map[string]int64, bytesMap map[string]int64) {
	sub := m.Subject
	if c.reportSub && m.Sub != nil {
		sub = m.Sub.Subject
	}
	mu.Lock()
	reportMap[sub]++
	bytesMap[sub] += int64(len(m.Data))
	mu.Unlock()
}

func (c *subCmd) handleGraphUpdate(m *nats.Msg, mu *sync.Mutex) {
	if m.Sub == nil {
		return
	}
	mu.Lock()
	c.messageRates[m.Sub.Subject].lastCount++
	mu.Unlock()
}

func (c *subCmd) handleMsg(m *nats.Msg, matchMap map[string]*nats.Msg, hcfg msgHandlerState) {
	if c.match && m.Reply != "" {
		matchMap[m.Reply] = m
	} else {
		c.printMsg(m, nil, *hcfg.counter, hcfg.startTime)
	}
}

func (c *subCmd) createMatchHandler(hcfg msgHandlerState, matchMap map[string]*nats.Msg) nats.MsgHandler {
	return func(reply *nats.Msg) {
		hcfg.msgMu.Lock()
		defer hcfg.msgMu.Unlock()

		request, ok := matchMap[reply.Subject]
		if !ok {
			return
		}

		c.printMsg(request, reply, *hcfg.counter, hcfg.startTime)
		delete(matchMap, reply.Subject)

		// if reached limit and matched all requests
		if *hcfg.counter == c.limit && len(matchMap) == 0 {
			if reply.Sub != nil {
				reply.Sub.Unsubscribe()
			}
			hcfg.cancelFn()
		}
	}
}

func (c *subCmd) graphSubscribe(ctx context.Context, nc *nats.Conn, handler nats.MsgHandler, subs *[]*nats.Subscription, subjMu *sync.Mutex) error {
	width, height, err := terminal.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return fmt.Errorf("failed to get terminal dimensions: %w", err)
	}
	if width < 20 || height < 20 {
		return fmt.Errorf("please increase terminal dimensions")
	}
	if width > 15 {
		width -= 10
	}
	if height > 10 {
		height -= 6
	}

	c.width = width
	c.height = height
	c.messageRates = make(map[string]*subMessageRate)
	for _, subject := range c.subjects {
		c.messageRates[subject] = &subMessageRate{
			rates: make([]float64, width),
		}
	}

	if len(c.subjects) > 4 {
		return fmt.Errorf("maximum 4 subject patterns may be graphed")
	}

	for _, subj := range c.subjects {
		sub, err := nc.Subscribe(subj, handler)
		if err != nil {
			return err
		}
		*subs = append(*subs, sub)
	}

	c.startGraph(ctx, subjMu)
	return nil
}

func (c *subCmd) reportSubscribe(ctx context.Context, nc *nats.Conn, handler nats.MsgHandler, subs *[]*nats.Subscription, subjMu *sync.Mutex, subjectReportMap map[string]int64, subjectBytesReportMap map[string]int64) error {
	for _, subj := range c.subjects {
		sub, err := nc.Subscribe(subj, handler)
		if err != nil {
			return err
		}
		*subs = append(*subs, sub)
	}

	c.startSubjectReporting(ctx, subjMu, subjectReportMap, subjectBytesReportMap, c.reportSubjectsCount)
	return nil
}

func (c *subCmd) jetStreamSubscribe(nc *nats.Conn, handler nats.MsgHandler, subs *[]*nats.Subscription, ignoreSubjects []string) error {
	js, err := nc.JetStream(jsOpts()...)
	if err != nil {
		return err
	}

	opts := []nats.SubOpt{
		nats.EnableFlowControl(),
		nats.IdleHeartbeat(5 * time.Second),
		nats.AckNone(),
	}

	if c.headersOnly || c.subjectsOnly {
		opts = append(opts, nats.HeadersOnly())
	}

	// Check if durable exists and adjust accordingly
	var bindDurable bool
	if c.durable != "" {
		con, err := js.ConsumerInfo(c.stream, c.durable)
		if err == nil {
			bindDurable = true
			c.jsAck = con.Config.AckPolicy != nats.AckNonePolicy
			log.Printf("Subscribing to JetStream Stream %q using existing durable %q", c.stream, c.durable)

			switch {
			case len(con.Config.FilterSubjects) > 1:
				return fmt.Errorf("cannot subscribe to multi filter consumers")
			case len(con.Config.FilterSubjects) == 1:
				c.subjects = con.Config.FilterSubjects
			case con.Config.FilterSubject != "":
				c.subjects = []string{con.Config.FilterSubject}
			}
		} else if errors.Is(err, nats.ErrConsumerNotFound) {
			opts = append(opts, nats.Durable(c.durable))
		} else {
			return err
		}
	}

	subMsg := c.firstSubject()
	if c.stream != "" {
		if len(c.subjects) == 0 {
			subMsg = f(c.streamObj.Subjects())
		}
		opts = append(opts, nats.BindStream(c.stream))
	}

	ignoredSubjInfo := ""
	if len(ignoreSubjects) > 0 {
		ignoredSubjInfo = fmt.Sprintf("\nIgnored subjects: %s", f(ignoreSubjects))
	}

	switch {
	case c.sseq > 0:
		log.Printf("Subscribing to JetStream Stream holding messages with subject %s starting with sequence %d %s", subMsg, c.sseq, ignoredSubjInfo)
		opts = append(opts, nats.StartSequence(c.sseq))
	case c.deliverLast:
		log.Printf("Subscribing to JetStream Stream holding messages with subject %s starting with the last message received %s", subMsg, ignoredSubjInfo)
		opts = append(opts, nats.DeliverLast())
	case c.deliverAll:
		log.Printf("Subscribing to JetStream Stream holding messages with subject %s starting with the first message received %s", subMsg, ignoredSubjInfo)
		opts = append(opts, nats.DeliverAll())
	case c.deliverNew:
		log.Printf("Subscribing to JetStream Stream holding messages with subject %s delivering any new messages received %s", subMsg, ignoredSubjInfo)
		opts = append(opts, nats.DeliverNew())
	case c.deliverSince != "":
		d, err := fisk.ParseDuration(c.deliverSince)
		if err != nil {
			return err
		}
		start := time.Now().Add(-1 * d)
		log.Printf("Subscribing to JetStream Stream holding messages with subject %s starting with messages since %s %s", subMsg, f(d), ignoredSubjInfo)
		opts = append(opts, nats.StartTime(start))
	case c.deliverLastPerSubject:
		log.Printf("Subscribing to JetStream Stream holding messages with subject %s for the last messages for each subject in the Stream %s", subMsg, ignoredSubjInfo)
		opts = append(opts, nats.DeliverLastPerSubject())
	}

	if bindDurable {
		sub, err := js.Subscribe(c.firstSubject(), handler, nats.Bind(c.stream, c.durable))
		if err != nil {
			return err
		}
		*subs = append(*subs, sub)
	} else {
		c.jsAck = false
		sub, err := js.Subscribe(c.firstSubject(), handler, opts...)
		if err != nil {
			return err
		}
		*subs = append(*subs, sub)
	}

	return nil
}

func (c *subCmd) queueSubscribe(nc *nats.Conn, handler nats.MsgHandler, subs *[]*nats.Subscription) error {
	sub, err := nc.QueueSubscribe(c.firstSubject(), c.queue, handler)
	if err != nil {
		return err
	}
	*subs = append(*subs, sub)
	return nil
}

func (c *subCmd) defaultSubscribe(nc *nats.Conn, handler nats.MsgHandler, subs *[]*nats.Subscription) error {
	for _, subj := range c.subjects {
		sub, err := nc.Subscribe(subj, handler)
		if err != nil {
			return err
		}
		*subs = append(*subs, sub)
	}
	return nil
}

func (c *subCmd) directSubscribe(subCtx context.Context, nc *nats.Conn, handler nats.MsgHandler, subs *[]*nats.Subscription, ignoreSubjects []string) error {
	_, js, err := prepareJSHelper()
	if err != nil {
		return err
	}

	ignoredSubjInfo := ""
	if len(ignoreSubjects) > 0 {
		ignoredSubjInfo = fmt.Sprintf("\nIgnored subjects: %s", f(ignoreSubjects))
	}

	subMsg := c.firstSubject()
	if c.stream != "" {
		if len(c.subjects) == 0 {
			subMsg = f(c.streamObj.Subjects())
		}
	}

	// GetBatch called later assumes that the first msg is at sequence 0. We get the real starting position here so
	// we can pass it on later
	streamState, err := c.streamObj.State()
	batchSequence := streamState.FirstSeq
	batchSize := 1
	lastForSubjects := false
	var start time.Time
	if err != nil {
		return err
	}

	switch {
	case c.sseq > 0:
		log.Printf("Subscribing to JetStream Stream (direct get) holding messages with subject %s starting with sequence %d %s", subMsg, c.sseq, ignoredSubjInfo)
		batchSequence = c.sseq
	case c.deliverLast:
		log.Printf("Subscribing to JetStream Stream (direct get) holding messages with subject %s starting with the last message received %s", subMsg, ignoredSubjInfo)
		batchSequence = streamState.LastSeq
	case c.deliverAll:
		log.Printf("Subscribing to JetStream Stream (direct get) holding messages with subject %s starting with the first message received %s", subMsg, ignoredSubjInfo)
		batchSequence = streamState.FirstSeq
	case c.deliverNew:
		log.Printf("Subscribing to JetStream Stream (direct get) holding messages with subject %s delivering any new messages received %s", subMsg, ignoredSubjInfo)
		batchSequence = streamState.LastSeq + 1
	case c.deliverSince != "":
		d, err := fisk.ParseDuration(c.deliverSince)
		if err != nil {
			return err
		}
		start = time.Now().Add(-1 * d)
		log.Printf("Subscribing to JetStream Stream (direct get) holding messages with subject %s starting with messages since %s %s", subMsg, f(d), ignoredSubjInfo)
		batchSequence = streamState.LastSeq + 1
	case c.deliverLastPerSubject:
		log.Printf("Subscribing to JetStream Stream (direct get) holding messages with subject %s for the last messages for each subject in the Stream %s", subMsg, ignoredSubjInfo)
		lastForSubjects = true
		batchSequence = streamState.LastSeq
	}

	for {
		var msgs iter.Seq2[*jetstream.RawStreamMsg, error]
		if lastForSubjects {
			msgs, err = jetstreamext.GetLastMsgsFor(subCtx, js, c.stream, c.subjects, jetstreamext.GetLastMsgsUpToSeq(batchSequence), jetstreamext.GetLastMsgsBatchSize(batchSize))
		} else {
			if !start.IsZero() {
				msgs, err = jetstreamext.GetBatch(subCtx, js, c.stream, batchSize, jetstreamext.GetBatchStartTime(start))
				// We only get the messages from start up to now, then we continue to use batchSequence
				start = time.Time{}
			} else {
				msgs, err = jetstreamext.GetBatch(subCtx, js, c.stream, batchSize, jetstreamext.GetBatchSeq(batchSequence))
			}
		}

		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}

	msgProcessing:
		for msg, err := range msgs {
			switch {
			case errors.Is(err, jetstreamext.ErrNoMessages):
				break msgProcessing
			case errors.Is(err, context.Canceled):
				return nil
			case err != nil:
				return err
			}

			// Put the relevant fields in a nats.Msg for printing. Omitting the Reply and Sub pointer will guard against
			// the handler functions doing something we can't do with a direct message
			nmsg := &nats.Msg{
				Subject: msg.Subject,
				Header:  msg.Header,
				Data:    msg.Data,
			}

			handler(nmsg)
			batchSequence += uint64(batchSize)
		}
	}
}

func (c *subCmd) subscribe(p *fisk.ParseContext) error {
	//nc, err := newNatsConn("", natsOpts()...)
	nc, mgr, err := prepareHelper("", natsOpts()...)
	if err != nil {
		return err
	}
	defer nc.Close()

	err = c.validateInputs(nc, mgr)
	if err != nil {
		return err
	}

	var (
		subs           []*nats.Subscription
		subjMu         = sync.Mutex{}
		ctr            = uint(0)
		ignoreSubjects = iu.SplitCLISubjects(c.ignoreSubjects)
		ctx, cancel    = context.WithCancel(ctx)
		dump           = c.dump != ""

		matchMap = make(map[string]*nats.Msg)

		subjectReportMap      map[string]int64
		subjectBytesReportMap map[string]int64

		startTime = time.Now()
	)

	hcfg := msgHandlerState{
		msgMu:     &sync.Mutex{},
		cancelFn:  cancel,
		counter:   &ctr,
		startTime: startTime,
	}

	defer cancel()

	// If the wait timeout is set, then we will cancel after the timer fires.
	var t *time.Timer
	if c.wait > 0 {
		t = time.AfterFunc(c.wait, func() {
			cancel()
		})
		defer t.Stop()
	}

	if c.reportSubjects {
		subjectReportMap = make(map[string]int64)
		subjectBytesReportMap = make(map[string]int64)
	}

	msgHandler := c.createMsgHandler(hcfg, nc, &subjMu, &subs, ignoreSubjects, subjectReportMap, subjectBytesReportMap, matchMap, t, dump)
	matchHandler := c.createMatchHandler(hcfg, matchMap)

	if c.match {
		inSubj := "_INBOX.>"
		if opts().InboxPrefix != "" {
			inSubj = fmt.Sprintf("%v.>", opts().InboxPrefix)
		}

		if !c.raw && c.dump == "" {
			log.Printf("Matching replies with inbox prefix %v", inSubj)
		}

		_, err = nc.Subscribe(inSubj, matchHandler)
		if err != nil {
			return err
		}
	}

	var ignoredSubjInfo string
	if len(ignoreSubjects) > 0 {
		ignoredSubjInfo = fmt.Sprintf("\nIgnored subjects: %s", f(ignoreSubjects))
	}

	if (!c.raw && c.dump == "") || c.inbox {
		switch {
		case c.jetStream:
			// logs later depending on settings
		case c.jsAck:
			log.Printf("Subscribing on %s with acknowledgement of JetStream messages %s", c.firstSubject(), ignoredSubjInfo)
		default:
			log.Printf("Subscribing on %s %s", strings.Join(c.subjects, ", "), ignoredSubjInfo)
		}
	}

	switch {
	case c.graphOnly:
		err = c.graphSubscribe(ctx, nc, msgHandler, &subs, &subjMu)
	case c.reportSubjects:
		err = c.reportSubscribe(ctx, nc, msgHandler, &subs, &subjMu, subjectReportMap, subjectBytesReportMap)
	case c.direct:
		err = c.directSubscribe(ctx, nc, msgHandler, &subs, ignoreSubjects)
	case c.jetStream:
		err = c.jetStreamSubscribe(nc, msgHandler, &subs, ignoreSubjects)
	case c.queue != "":
		err = c.queueSubscribe(nc, msgHandler, &subs)
	default:
		err = c.defaultSubscribe(nc, msgHandler, &subs)
	}
	if err != nil {
		return err
	}

	nc.Flush()

	err = nc.LastError()
	if err != nil {
		return err
	}

	<-ctx.Done()

	return nil
}

func (c *subCmd) firstSubject() string {
	if len(c.subjects) == 0 {
		return ""
	}

	return c.subjects[0]
}

func (c *subCmd) printMsg(msg *nats.Msg, reply *nats.Msg, ctr uint, startTime time.Time) {
	var info *jsm.MsgInfo
	if msg.Reply != "" || c.direct {
		info, _ = jsm.ParseJSMsgMetadata(msg)
	}

	if opts().Trace && msg.Reply != "" {
		fmt.Printf("<<< Reply Subject: %v\n", msg.Reply)
	}

	var timeStamp string
	if c.timeStamps {
		timeStamp = fmt.Sprintf(" @ %s", time.Now().Format(time.StampMicro))
	} else if c.deltaTimeStamps {
		timeStamp = fmt.Sprintf(" @ %s", time.Since(startTime).String())
	}

	if c.dump != "" {
		// Output format 1: dumping, to stdout or files

		var (
			stdout      = c.dump == "-"
			requestFile string
			replyFile   string
		)
		if !stdout {
			if info == nil {
				requestFile = filepath.Join(c.dump, fmt.Sprintf("%d.json", ctr))
				replyFile = filepath.Join(c.dump, fmt.Sprintf("%d_reply.json", ctr))
			} else {
				requestFile = filepath.Join(c.dump, fmt.Sprintf("%d.json", info.StreamSequence()))
				replyFile = filepath.Join(c.dump, fmt.Sprintf("%d_reply.json", info.StreamSequence()))
			}
		}

		c.dumpMsg(msg, stdout, requestFile, ctr)
		if reply != nil {
			c.dumpMsg(reply, stdout, replyFile, ctr)
		}

	} else if c.raw {
		// Output format 2: raw
		outPutMSGBodyCompact(msg.Data, c.translate, "", "")
		if reply != nil {
			fmt.Println(string(reply.Data))
		}

	} else {
		// Output format 3: pretty

		if info == nil {
			if msg.Reply != "" {
				fmt.Printf("[#%d]%s Received on %q with reply %q\n", ctr, timeStamp, msg.Subject, msg.Reply)
			} else {
				fmt.Printf("[#%d]%s Received on %q\n", ctr, timeStamp, msg.Subject)
			}
		} else if c.jetStream {
			fmt.Printf("[#%d] Received JetStream message: stream: %s seq %d / subject: %s / time: %v\n", ctr, info.Stream(), info.StreamSequence(), msg.Subject, info.TimeStamp().UTC().Format(time.RFC3339))
		} else {
			fmt.Printf("[#%d] Received JetStream message: consumer: %s > %s / subject: %s / delivered: %d / consumer seq: %d / stream seq: %d\n", ctr, info.Stream(), info.Consumer(), msg.Subject, info.Delivered(), info.ConsumerSequence(), info.StreamSequence())
		}

		if c.subjectsOnly {
			return
		}

		c.prettyPrintMsg(msg, c.headersOnly, c.translate)

		if reply != nil {
			if info == nil {
				fmt.Printf("[#%d]%s Matched reply on %q\n", ctr, timeStamp, reply.Subject)
			} else if c.jetStream {
				fmt.Printf("[#%d] Matched reply JetStream message: stream: %s seq %d / subject: %s / time: %v\n", ctr, info.Stream(), info.StreamSequence(), reply.Subject, info.TimeStamp().Format(time.RFC3339))
			} else {
				fmt.Printf("[#%d] Matched reply JetStream message: consumer: %s > %s / subject: %s / delivered: %d / consumer seq: %d / stream seq: %d\n", ctr, info.Stream(), info.Consumer(), reply.Subject, info.Delivered(), info.ConsumerSequence(), info.StreamSequence())
			}

			c.prettyPrintMsg(reply, c.headersOnly, c.translate)

		}
	} // output format type dispatch
}

func (c *subCmd) dumpMsg(msg *nats.Msg, stdout bool, filepath string, ctr uint) {
	// dont want sub etc
	serMsg := nats.NewMsg(msg.Subject)
	serMsg.Header = msg.Header
	serMsg.Data = msg.Data
	serMsg.Reply = msg.Reply

	if c.translate != "" {
		data, err := filterDataThroughCmd(msg.Data, c.translate, "", "")
		if err != nil {
			log.Printf("%q\nError while translating msg body: %s\n\n", data, err.Error())
			return
		}
		serMsg.Data = data
	}

	jm, err := json.Marshal(serMsg)
	if err != nil {
		log.Printf("Could not JSON encode message: %s", err)
	} else if stdout {
		os.Stdout.WriteString(fmt.Sprintf("%s\000", jm))
	} else {
		err = os.WriteFile(filepath, jm, 0600)
		if err != nil {
			log.Printf("Could not save message: %s", err)
		}

		if ctr%100 == 0 {
			fmt.Print(".")
		}
	}
}

func (c *subCmd) prettyPrintMsg(msg *nats.Msg, headersOnly bool, filter string) {
	if len(msg.Header) > 0 {
		for h, vals := range msg.Header {
			for _, val := range vals {
				fmt.Printf("%s: %s\n", h, val)
			}
		}

		fmt.Println()
	}

	if !headersOnly {
		outPutMSGBody(msg.Data, filter, msg.Subject, "")
	}
}
