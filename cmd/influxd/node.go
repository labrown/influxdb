package main

import (
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/influxdb/influxdb"
	"github.com/influxdb/influxdb/admin"
	"github.com/influxdb/influxdb/graphite"
	"github.com/influxdb/influxdb/messaging"
	"github.com/influxdb/influxdb/opentsdb"
	"github.com/influxdb/influxdb/raft"
)

// Node represent a member of a cluster.  A Node could serve as broker, a data node
// or both.
type Node struct {
	Broker   *influxdb.Broker
	DataNode *influxdb.Server
	raftLog  *raft.Log
	config   *Config

	hostname string

	adminServer     *admin.Server
	clusterListener net.Listener      // The cluster TCP listener
	apiListener     net.Listener      // The API TCP listener
	GraphiteServers []graphite.Server // The Graphite Servers
	OpenTSDBServer  *opentsdb.Server  // The OpenTSDB Server
}

func NewNodeWithConfig(config *Config) *Node {
	return &Node{config: config}
}

// ClusterAddr returns the cluster listen address
func (s *Node) ClusterAddr() net.Addr {
	return s.clusterListener.Addr()
}

// ClusterURL returns the URL where other members can reach this node
func (s *Node) ClusterURL() *url.URL {
	// Find out which port the cluster started on
	_, p, e := net.SplitHostPort(s.ClusterAddr().String())
	if e != nil {
		panic(e)
	}

	h := net.JoinHostPort(s.hostname, p)
	return &url.URL{
		Scheme: "http",
		Host:   h,
	}
}

// creates and initializes a broker.
func (s *Node) openBroker(brokerURLs []url.URL, h *Handler) {
	path := s.config.BrokerDir()
	u := s.ClusterURL()
	raftTracing := s.config.Logging.RaftTracing

	// Create broker
	b := influxdb.NewBroker()
	b.TruncationInterval = time.Duration(s.config.Broker.TruncationInterval)
	b.MaxTopicSize = s.config.Broker.MaxTopicSize
	b.MaxSegmentSize = s.config.Broker.MaxSegmentSize
	s.Broker = b

	// Create raft log.
	l := raft.NewLog()
	l.SetURL(*u)
	l.DebugEnabled = raftTracing
	b.Log = l
	s.raftLog = l

	// Create Raft clock.
	clk := raft.NewClock()
	clk.ApplyInterval = time.Duration(s.config.Raft.ApplyInterval)
	clk.ElectionTimeout = time.Duration(s.config.Raft.ElectionTimeout)
	clk.HeartbeatInterval = time.Duration(s.config.Raft.HeartbeatInterval)
	clk.ReconnectTimeout = time.Duration(s.config.Raft.ReconnectTimeout)
	l.Clock = clk

	// Open broker so it can feed last index data to the log.
	if err := b.Open(path); err != nil {
		log.Fatalf("failed to open broker at %s : %s", path, err)
	}
	log.Printf("broker opened at %s", path)

	// Attach the broker as the finite state machine of the raft log.
	l.FSM = &messaging.RaftFSM{Broker: b}

	// Open raft log inside broker directory.
	if err := l.Open(filepath.Join(path, "raft")); err != nil {
		log.Fatalf("raft: %s", err)
	}

	// Attach broker and log to handler.
	h.Broker = b
	h.Log = l

	// Checks to see if the raft index is 0.  If it's 0, it might be the first
	// node in the cluster and must initialize or join
	index, _ := l.LastLogIndexTerm()
	if index == 0 {
		// If we have join URLs, then attemp to join the cluster
		if len(brokerURLs) > 0 {
			s.joinLog(l, brokerURLs)
			return
		}

		if err := l.Initialize(); err != nil {
			log.Fatalf("initialize raft log: %s", err)
		}

		u := b.Broker.URL()
		log.Printf("initialized broker: %s\n", (&u).String())
	} else {
		log.Printf("broker already member of cluster.  Using existing state and ignoring join URLs")
	}
}

// joins a raft log to an existing cluster.
func (n *Node) joinLog(l *raft.Log, brokerURLs []url.URL) {
	// Attempts to join each server until successful.
	for _, u := range brokerURLs {
		if err := l.Join(u); err == raft.ErrInitialized {
			return
		} else if err != nil {
			log.Printf("join: failed to connect to raft cluster: %s: %s", (&u).String(), err)
		} else {
			log.Printf("join: connected raft log to %s", (&u).String())
			return
		}
	}
	log.Fatalf("join: failed to connect raft log to any specified server")
}

// Close stops all listeners and services on the node
func (s *Node) Close() error {
	if err := s.closeClusterListener(); err != nil {
		return err
	}

	if err := s.closeAPIListener(); err != nil {
		return err
	}

	if err := s.closeAdminServer(); err != nil {
		return err
	}

	for _, g := range s.GraphiteServers {
		if err := g.Close(); err != nil {
			return err
		}
	}

	if s.OpenTSDBServer != nil {
		if err := s.OpenTSDBServer.Close(); err != nil {
			return err
		}
	}

	if s.DataNode != nil {
		if err := s.DataNode.Close(); err != nil {
			return err
		}
	}

	if s.raftLog != nil {
		if err := s.raftLog.Close(); err != nil {
			return err
		}
	}

	if s.Broker != nil {
		if err := s.Broker.Close(); err != nil {
			return err
		}
	}

	return nil
}

// creates and initializes a server.
func (s *Node) openServer(joinURLs []url.URL) *influxdb.Server {

	// Create messaging client to the brokers.
	c := influxdb.NewMessagingClient(*s.ClusterURL())
	c.SetURLs(joinURLs)

	if err := c.Open(filepath.Join(s.config.Data.Dir, messagingClientFile)); err != nil {
		log.Fatalf("messaging client error: %s", err)
	}

	// If no URLs exist on the client the return an error since we cannot reach a broker.
	if len(c.URLs()) == 0 {
		log.Fatal("messaging client has no broker URLs")
	}

	// Create and open the server.
	n := influxdb.NewServer()

	n.WriteTrace = s.config.Logging.WriteTracing
	n.RetentionAutoCreate = s.config.Data.RetentionAutoCreate
	n.RecomputePreviousN = s.config.ContinuousQuery.RecomputePreviousN
	n.RecomputeNoOlderThan = time.Duration(s.config.ContinuousQuery.RecomputeNoOlderThan)
	n.ComputeRunsPerInterval = s.config.ContinuousQuery.ComputeRunsPerInterval
	n.ComputeNoMoreThan = time.Duration(s.config.ContinuousQuery.ComputeNoMoreThan)
	n.Version = version
	n.CommitHash = commit

	// Open server with data directory and broker client.
	if err := n.Open(s.config.Data.Dir, c); err != nil {
		log.Fatalf("failed to open data node: %v", err.Error())
	}
	log.Printf("data node(%d) opened at %s", n.ID(), s.config.Data.Dir)

	// Give brokers time to elect a leader if entire cluster is being restarted.
	time.Sleep(1 * time.Second)

	if n.ID() == 0 {
		s.joinOrInitializeServer(n, *s.ClusterURL(), joinURLs)
	} else {
		log.Printf("data node already member of cluster. Using existing state and ignoring join URLs")
	}

	return n
}

// joinOrInitializeServer joins a new server to an existing cluster or initializes it as the first
// member of the cluster
func (s *Node) joinOrInitializeServer(n *influxdb.Server, u url.URL, joinURLs []url.URL) {
	// Create data node on an existing data node.
	for _, joinURL := range joinURLs {
		if err := n.Join(&u, &joinURL); err == influxdb.ErrDataNodeNotFound {
			// No data nodes could be found to join.  We're the first.
			if err := n.Initialize(u); err != nil {
				log.Fatalf("server initialization error(1): %s", err)
			}
			log.Printf("initialized data node: %s\n", (&u).String())
			return
		} else if err != nil {
			// does not return so that the next joinURL can be tried
			log.Printf("join: failed to connect data node: %s: %s", (&u).String(), err)
		} else {
			log.Printf("join: connected data node to %s", u)
			return
		}
	}

	if len(joinURLs) == 0 {
		if err := n.Initialize(u); err != nil {
			log.Fatalf("server initialization error(2): %s", err)
		}
		log.Printf("initialized data node: %s\n", (&u).String())
		return
	}

	log.Fatalf("join: failed to connect data node to any specified server")
}

func (s *Node) openAdminServer(port int) error {
	// Start the admin interface on the default port
	addr := net.JoinHostPort("", strconv.Itoa(port))
	s.adminServer = admin.NewServer(addr)
	return s.adminServer.ListenAndServe()
}

func (s *Node) closeAdminServer() error {
	if s.adminServer != nil {
		return s.adminServer.Close()
	}
	return nil
}

func (s *Node) openListener(desc, addr string, h http.Handler) (net.Listener, error) {
	var err error
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		err := http.Serve(listener, h)

		// The listener was closed so exit
		// See https://github.com/golang/go/issues/4373
		if strings.Contains(err.Error(), "closed") {
			return
		}
		if err != nil {
			log.Fatalf("%s server failed to serve on %s: %s", desc, addr, err)
		}
	}()
	return listener, nil

}

func (s *Node) openAPIListener(addr string, h http.Handler) error {
	var err error
	s.apiListener, err = s.openListener("API", addr, h)
	if err != nil {
		return err
	}
	return nil
}

func (s *Node) closeAPIListener() error {
	var err error
	if s.apiListener != nil {
		err = s.apiListener.Close()
		s.apiListener = nil
	}
	return err
}

func (s *Node) openClusterListener(addr string, h http.Handler) error {
	var err error
	s.clusterListener, err = s.openListener("Cluster", addr, h)
	if err != nil {
		return err
	}
	return nil
}

func (s *Node) closeClusterListener() error {
	var err error
	if s.clusterListener != nil {
		err = s.clusterListener.Close()
		s.clusterListener = nil
	}
	return err
}
