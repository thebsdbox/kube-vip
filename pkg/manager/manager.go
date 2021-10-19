package manager

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/kamhlos/upnp"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

const plunderLock = "plndr-svcs-lock"

// Manager degines the manager of the load-balancing services
type Manager struct {
	clientSet *kubernetes.Clientset
	configMap string
	config    *kubevip.Config

	// Manager services
	service bool

	// Keeps track of all running instances
	serviceInstances []Instance

	// Additional functionality
	upnp *upnp.Upnp

	//BGP Manager, this is a singleton that manages all BGP advertisements
	bgpServer *bgp.Server

	// This channel is used to signal a shutdown
	signalChan chan os.Signal

	// This is a promethues counter used to count the number of events received
	// from the service watcher
	countServiceWatchEvent *prometheus.CounterVec
}

// Instance defines an instance of everything needed to manage a vip
type Instance struct {
	// Virtual IP / Load Balancer configuration
	vipConfig kubevip.Config

	// cluster instance
	cluster cluster.Cluster

	// Service uses DHCP
	isDHCP              bool
	dhcpInterface       string
	dhcpInterfaceHwaddr string
	dhcpInterfaceIP     string
	dhcpClient          *vip.DHCPClient

	// Kubernetes service mapping
	Vip  string
	Port int32
	UID  string
	Type string

	ServiceName string
}

// New will create a new managing object
func New(configMap string, config *kubevip.Config) (*Manager, error) {
	var clientset *kubernetes.Clientset

	var configPath string
	var cfg *rest.Config
	var err error

	configPath = "/etc/kubernetes/admin.conf"
	if fileExists(configPath) {
		cfg, err = clientcmd.BuildConfigFromFlags("", configPath)
		if err != nil {
			return nil, err
		}
		log.Debugf("Using external Kubernetes configuration from file [%s]", configPath)
	} else {
		// Second check in home directory for kube config
		configPath = filepath.Join(os.Getenv("HOME"), ".kube", "config")
		if fileExists(configPath) {
			log.Debugf("Using external Kubernetes configuration from file [%s]", configPath)
			cfg, err = clientcmd.BuildConfigFromFlags("", configPath)
			if err != nil {
				return nil, err
			}
		} else {
			cfg, err = rest.InClusterConfig()
			if err != nil {
				return nil, fmt.Errorf("error creating kubernetes client config: %s", err.Error())
			}
			log.Debugf("Using the internal Kubernetes token")
		}
	}

	clientset, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes client: %s", err.Error())
	}
	// If this is a control pane host it will likely have started as a static pod or wont have the
	// VIP up before trying to connect to the API server, we set the API endpoint to this machine to
	// ensure connectivity.
	if config.EnableControlPane {
		log.Debugf("Modifying address of Kubernetes server to localhost")
		cfg.Host = fmt.Sprintf("kubernetes:%v", config.Port)
		clientset, err = kubernetes.NewForConfig(cfg)
	}
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes client: %s", err.Error())
	}

	return &Manager{
		clientSet: clientset,
		configMap: configMap,
		config:    config,
		countServiceWatchEvent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "kube_vip",
			Subsystem: "manager",
			Name:      "all_services_events",
			Help:      "Count all events fired by the service watcher categorised by event type",
		}, []string{"type"}),
	}, nil
}

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) Start() error {

	// listen for interrupts or the Linux SIGTERM signal and cancel
	// our context, which the leader election code will observe and
	// step down
	sm.signalChan = make(chan os.Signal, 1)
	// Add Notification for Userland interrupt
	signal.Notify(sm.signalChan, syscall.SIGINT)

	// Add Notification for SIGTERM (sent from Kubernetes)
	signal.Notify(sm.signalChan, syscall.SIGTERM)

	// Add Notification for SIGKILL (sent from Kubernetes)
	signal.Notify(sm.signalChan, syscall.SIGKILL)

	// If BGP is enabled then we start a server instance that will broadcast VIPs
	if sm.config.EnableBGP {

		// If Annotations have been set then we will look them up
		err := sm.parseAnnotations()
		if err != nil {
			return err
		}

		log.Infoln("Starting Kube-vip Manager with the BGP engine")
		log.Infof("Namespace [%s], Hybrid mode [%t]", sm.config.Namespace, sm.config.EnableControlPane && sm.config.EnableServices)
		return sm.startBGP()
	}

	// If ARP is enabled then we start a LeaderElection that will use ARP to advertise VIPs
	if sm.config.EnableARP {
		log.Infoln("Starting Kube-vip Manager with the ARP engine")
		log.Infof("Namespace [%s], Hybrid mode [%t]", sm.config.Namespace, sm.config.EnableControlPane && sm.config.EnableServices)
		return sm.startARP()
	}

	// If ARP is enabled then we start a LeaderElection that will use ARP to advertise VIPs
	if sm.config.EnableVRRP {
		log.Infoln("Starting Kube-vip Manager with the VRRP engine")
		log.Infof("Namespace [%s], Hybrid mode [%t]", sm.config.Namespace, sm.config.EnableControlPane && sm.config.EnableServices)
		return sm.startVRRP()
	}

	log.Infoln("Prematurely exiting Load-balancer as neither Layer2 or Layer3 is enabled")
	return nil
}

func returnNameSpace() (string, error) {
	if data, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns, nil
		}
		return "", err
	}
	return "", fmt.Errorf("Unable to find Namespace")
}

func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

func (sm *Manager) parseAnnotations() error {
	if sm.config.Annotations == "" {
		log.Debugf("No Node annotations to parse")
		return nil
	}

	err := sm.annotationsWatcher()
	if err != nil {
		return err
	}
	return nil
}
