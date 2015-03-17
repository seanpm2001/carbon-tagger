package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"github.com/Dieterbe/go-metrics"
	"github.com/mattbaird/elastigo/api"
	"github.com/mattbaird/elastigo/core"
	"github.com/stvp/go-toml-config"
	"io"
	"net"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"
)

func dieIfError(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "Fatal error: %s\n", err.Error())
		os.Exit(1)
	}
}

var (
	cpuprofile = flag.String("cpuprofile", "", "write cpu profile to file")
	memprofile = flag.String("memprofile", "", "write memory profile to this file")
	configFile = flag.String("config", "carbon-tagger.conf", "config file")

	es_host        = config.String("elasticsearch.host", "undefined")
	es_port        = config.Int("elasticsearch.port", 9200)
	es_index_name  = config.String("elasticsearch.index", "graphite_metrics2")
	es_max_pending = config.Int("elasticsearch.max_pending", 1000000)
	in_port        = config.Int("in.port", 2003)
	out_host       = config.String("out.host", "localhost")
	out_port       = config.Int("out.port", 2005)

	stats_id             *string
	stats_flush_interval *int

	in_conns_current             stat
	in_conns_broken_total        stat
	in_metrics_proto1_good_total stat
	in_metrics_proto2_good_total stat
	in_metrics_proto1_bad_total  stat
	in_metrics_proto2_bad_total  stat
	num_metrics_to_track         stat // backlog in our queue (excl elastigo queue)
	num_seen_proto2              stat
	num_seen_proto1              stat

	lines_read  chan []byte
	proto1_read chan string
	proto2_read chan metricSpec
)

func main() {
	flag.Parse()
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		dieIfError(err)
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
		fmt.Println("cpuprof on")
	}
	if *memprofile != "" {
		f, err := os.Create(*memprofile)
		dieIfError(err)
		defer f.Close()
		defer pprof.WriteHeapProfile(f)
	}

	stats_id = config.String("stats.id", "myhost")
	stats_flush_interval = config.Int("stats.flush_interval", 10)
	err := config.Parse(*configFile)
	dieIfError(err)

	in_conns_current = NewGauge("unit_is_Conn.direction_is_in.type_is_open", false)
	in_conns_broken_total = NewCounter("unit_is_Conn.direction_is_in.type_is_broken", false)
	in_metrics_proto1_good_total = NewCounter("unit_is_Metric.proto_is_1.direction_is_in.type_is_good", false) // no thorough check
	in_metrics_proto2_good_total = NewCounter("unit_is_Metric.proto_is_2.direction_is_in.type_is_good", false)
	in_metrics_proto1_bad_total = NewCounter("unit_is_Err.type_is_invalid_line.proto_is_1.direction_is_in", false)
	in_metrics_proto2_bad_total = NewCounter("unit_is_Err.type_is_invalid_line.proto_is_2.direction_is_in", false)
	num_metrics_to_track = NewCounter("unit_is_Metric.proto_is_2.type_is_to_track", true)
	num_seen_proto1 = NewGauge("unit_is_Metric.proto_is_1.type_is_tracked", true)
	num_seen_proto2 = NewGauge("unit_is_Metric.proto_is_2.type_is_tracked", true)

	lines_read = make(chan []byte)
	proto1_read = make(chan string)
	// we can queue up to max_pending: if more than that are pending flush to ES, start blocking..
	proto2_read = make(chan metricSpec, *es_max_pending)

	// connect to elasticsearch database to store tags
	api.Domain = *es_host
	api.Port = strconv.Itoa(*es_port)
	done := make(chan bool)
	indexer := core.NewBulkIndexer(4)
	indexer.Run(done)

	go processInputLines()
	go trackProto1()
	// 1 worker, but ES library has multiple workers
	go trackProto2(indexer, *es_index_name)

	statsAddr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:%d", *out_host, *out_port))
	dieIfError(err)
	go metrics.Graphite(metrics.DefaultRegistry, time.Duration(*stats_flush_interval)*time.Second, "", statsAddr)

	// listen for incoming metrics
	addr, err := net.ResolveTCPAddr("tcp4", fmt.Sprintf(":%d", *in_port))
	dieIfError(err)
	listener, err := net.ListenTCP("tcp", addr)
	dieIfError(err)
	defer listener.Close()
	fmt.Printf("carbon-tagger %s listening on %d\n", *stats_id, *in_port)
	for {
		// would be nice to have a metric showing highest amount of connections seen per interval
		conn_in, err := listener.Accept()
		if err != nil {
			fmt.Fprint(os.Stderr, err)
			continue
		}
		go handleClient(conn_in)
	}
}

func parseTagBasedMetric(metric_line string) (metric metricSpec, err error) {
	// metric_spec value unix_timestamp
	elements := strings.Split(metric_line, " ")
	metric_id := ""
	if len(elements) != 3 {
		return metricSpec{metric_id, nil}, errors.New(fmt.Sprintf("metric doesn't contain exactly 3 nodes: %s", metric_line))
	}
	metric_id = elements[0]
	nodes := strings.Split(metric_id, ".")
	tags := make(map[string]string)
	for i, node := range nodes {
		var tag []string
		if strings.Contains(node, "_is_") {
			tag = strings.Split(node, "_is_")
		} else {
			tag = strings.Split(node, "=")
		}
		if len(tag) > 2 {
			return metricSpec{metric_id, nil}, errors.New("bad metric spec: more than 1 equals")
		} else if len(tag) < 2 {
			tags[fmt.Sprintf("n%d", i+1)] = node
		} else if tag[0] == "" || tag[1] == "" {
			return metricSpec{metric_id, nil}, errors.New("bad metric spec: tag_k and tag_v must be non-empty strings")
		} else {
			// k=v format, and both are != ""
			tags[tag[0]] = tag[1]
		}
	}
	if u, ok := tags["unit"]; !ok {
		return metricSpec{metric_id, nil}, errors.New("bad metric spec: unit tag (mandatory) not specified")
	} else if strings.HasSuffix(u, "ps") {
		tags["unit"] = u[:len(u)-2] + "/s"
	}

	if len(tags) < 2 {
		return metricSpec{metric_id, nil}, errors.New("bad metric spec: must have at least one tag_k/tag_v pair beyond unit")
	}
	return metricSpec{metric_id, tags}, nil
}

func handleClient(conn_in net.Conn) {
	in_conns_current.Inc(1)
	defer in_conns_current.Dec(1)
	defer conn_in.Close()
	reader := bufio.NewReader(conn_in)
	for {
		// TODO handle isPrefix cases (means we should merge this read with the next one in a different packet, i think)
		buf, err := reader.ReadBytes('\n')
		if err != nil {
			str := strings.TrimSpace(string(buf))
			if err != io.EOF {
				fmt.Printf("WARN connection closed uncleanly/broken: %s\n", err.Error())
				in_conns_broken_total.Inc(1)
			}
			if len(str) > 0 {
				// todo handle incomplete reads
				fmt.Printf("WARN incomplete read, line read: '%s'. neglecting line because connection closed because of %s\n", str, err.Error())
			}
			return
		}
		lines_read <- buf
	}
}

func processInputLines() {
	equals1 := []byte("=")
	equals2 := []byte("_is_")
	for buf := range lines_read {
		str := string(buf)
		if bytes.Contains(buf, equals1) || bytes.Contains(buf, equals2) {
			str = strings.TrimSpace(str)
			metric, err := parseTagBasedMetric(str)
			if err != nil {
				in_metrics_proto2_bad_total.Inc(1)
			} else {
				in_metrics_proto2_good_total.Inc(1)
				proto2_read <- metric
			}
		} else {
			elements := strings.Split(str, " ")
			if len(elements) == 3 {
				in_metrics_proto1_good_total.Inc(1)
				proto1_read <- str
			} else {
				in_metrics_proto1_bad_total.Inc(1)
			}
		}
	}
}

func trackProto1() {
	seen := make(map[string]bool)
	for {
		select {
		case buf := <-proto1_read:
			seen[buf] = true
		case <-num_seen_proto1.valueReq:
			num_seen_proto1.valueResp <- int64(len(seen))
			seen = make(map[string]bool)
		}
	}
}

func trackProto2(indexer *core.BulkIndexer, index_name string) {
	seen := make(map[string]bool)  // for ES. seen once = never need to resubmit
	seen2 := make(map[string]bool) // for stats, provides "how many recently seen?"
	for {
		select {
		case metric := <-proto2_read:
			seen2[metric.metric_id] = true
			if _, ok := seen[metric.metric_id]; ok {
				continue
			}
			date := time.Now()
			refresh := false // we can wait until the regular indexing runs
			metric_es := NewMetricEs(metric)
			err := indexer.Index(index_name, "metric", metric.metric_id, "", &date, &metric_es, refresh)
			dieIfError(err)
			seen[metric.metric_id] = true
		case <-num_metrics_to_track.valueReq:
			num_metrics_to_track.valueResp <- int64(len(proto2_read))
		case <-num_seen_proto2.valueReq:
			num_seen_proto2.valueResp <- int64(len(seen2))
			seen2 = make(map[string]bool)
		}
	}
}
