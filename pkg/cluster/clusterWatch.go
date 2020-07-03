package cluster

import (
	"fmt"
	"os"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"github.com/hashicorp/raft"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	watchtools "k8s.io/client-go/tools/watch"
)

func (cluster *Cluster) newNodeWatcher(host string) error {

	kubeConfigPath := "/etc/kubernetes/admin.conf"

	if _, err := os.Stat(kubeConfigPath); os.IsNotExist(err) {
		log.Fatalf("Unable to find file [%s]", kubeConfigPath)
	}

	// We will use kubeconfig in order to find all the master nodes
	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfigPath)
	if err != nil {
		log.Fatal(err.Error())
	}

	// Set loopback
	config.Host = fmt.Sprintf("%s:6443", host)

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err.Error())
	}

	opts := metav1.ListOptions{}
	opts.LabelSelector = "node-role.kubernetes.io/master"

	// Watch function
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	cluster.rw, err = watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return clientset.CoreV1().Nodes().Watch(opts)
		},
	})

	if err != nil {
		return fmt.Errorf("error creating watcher: %s", err.Error())
	}

	ch := cluster.rw.ResultChan()
	//defer cluster.rw.Stop()
	log.Infof("Beginning watching Kubernetes Control Plane Nodes")

	for event := range ch {

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			node, ok := event.Object.(*v1.Node)
			if !ok {
				return fmt.Errorf("Unable to parse Node from watcher")
			}

			var nodeAddress, nodeHostname string
			for y := range node.Status.Addresses {
				switch node.Status.Addresses[y].Type {
				case corev1.NodeHostName:
					nodeHostname = node.Status.Addresses[y].Address
				case corev1.NodeInternalIP:
					nodeAddress = node.Status.Addresses[y].Address
				}
			}

			go func() {
				log.Infof("Watching node [%s] for kube-vip pod", node.Name)
				podrw, err := watchtools.NewRetryWatcher("1", &cache.ListWatch{
					WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
						return clientset.CoreV1().Pods("").Watch(metav1.ListOptions{
							FieldSelector: "spec.nodeName=" + node.Name,
						})
					},
				})

				if err != nil {
					log.Errorf("error creating watcher: %s", err.Error())
					return
				}

				podch := podrw.ResultChan()

				// Check for a kube-vip pod

				for event := range podch {

					// We need to inspect the event and get ResourceVersion out of it
					switch event.Type {
					case watch.Added, watch.Modified:
						pods, ok := event.Object.(*v1.Pod)
						if !ok {
							fmt.Printf("%v\n", event.Object)
							log.Errorf("Unable to parse Pods from watcher")
							return
						}

						//log.Info("Querying [%d] pods", len(pods.Items))
						//for y := range pods.Items {
						if strings.Contains(pods.Name, "kube-vip") {

							// Retrieve the current configuration and find this server
							var found bool
							for x := range cluster.raftServer.GetConfiguration().Configuration().Servers {
								if cluster.raftServer.GetConfiguration().Configuration().Servers[x].ID == raft.ServerID(nodeHostname) {
									found = true
								}
							}
							if !found {
								log.Infof("New Node [%s] and peer [%s:10000]", nodeHostname, nodeAddress)
								cluster.AddNode(nodeHostname, fmt.Sprintf("%s:10000", nodeAddress))
							}
						}

						//}
					case watch.Deleted:

						//return nil
					case watch.Bookmark:
						// Un-used
					case watch.Error:
						log.Infoln("err")

						// This round trip allows us to handle unstructured status
						errObject := apierrors.FromObject(event.Object)
						statusErr, ok := errObject.(*apierrors.StatusError)
						if !ok {
							log.Errorf(spew.Sprintf("Received an error which is not *metav1.Status but %#+v", event.Object))
							// Retry unknown errors
							//return false, 0
						}

						status := statusErr.ErrStatus
						log.Errorf("%v", status)
					default:
					}
				}
				log.Warnln("Stopping watching Kubernetes Node for vip pod")
			}()
			// for y := range pods.Items {
			// 	if strings.Contains(pods.Items[y].Name, "kube-vip") {

			// 		// Retrieve the current configuration and find this server
			// 		var found bool
			// 		for x := range cluster.raftServer.GetConfiguration().Configuration().Servers {
			// 			if cluster.raftServer.GetConfiguration().Configuration().Servers[x].ID == raft.ServerID(nodeHostname) {
			// 				found = true
			// 			}
			// 		}
			// 		if !found {
			// 			log.Infof("New Node [%s] and peer [%s:10000]", nodeHostname, nodeAddress)
			// 			cluster.AddNode(nodeHostname, fmt.Sprintf("%s:10000", nodeAddress))
			// 		}
			// 	}

			// }

		case watch.Deleted:
			node, ok := event.Object.(*v1.Node)
			if !ok {
				return fmt.Errorf("Unable to parse Node from watcher")
			}
			var nodeAddress, nodeHostname string
			for y := range node.Status.Addresses {
				switch node.Status.Addresses[y].Type {
				case corev1.NodeHostName:
					nodeHostname = node.Status.Addresses[y].Address
				case corev1.NodeInternalIP:
					nodeAddress = node.Status.Addresses[y].Address
				}
			}
			// Retrieve the current configuration and find this server
			var found bool
			for x := range cluster.raftServer.GetConfiguration().Configuration().Servers {
				if cluster.raftServer.GetConfiguration().Configuration().Servers[x].ID == raft.ServerID(nodeHostname) {
					found = true
				}
			}
			if found {
				log.Infof("Deleting Node [%s] and peer [%s:10000]", nodeHostname, nodeAddress)
				err = cluster.DelNode(nodeHostname)
				log.Errorln(err)
			}

			//return nil
		case watch.Bookmark:
			// Un-used
		case watch.Error:
			log.Infoln("err")

			// This round trip allows us to handle unstructured status
			errObject := apierrors.FromObject(event.Object)
			statusErr, ok := errObject.(*apierrors.StatusError)
			if !ok {
				log.Errorf(spew.Sprintf("Received an error which is not *metav1.Status but %#+v", event.Object))
				// Retry unknown errors
				//return false, 0
			}

			status := statusErr.ErrStatus
			log.Errorf("%v", status)
		default:
		}
	}
	log.Warnln("Stopping watching Kubernetes Control Plane Nodes")

	return nil
}

func (cluster *Cluster) newKubeadmWatcher(host string) error {

	kubeConfigPath := "/etc/kubernetes/admin.conf"

	if _, err := os.Stat(kubeConfigPath); os.IsNotExist(err) {
		log.Fatalf("Unable to find file [%s]", kubeConfigPath)
	}

	// We will use kubeconfig in order to find all the master nodes
	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfigPath)
	if err != nil {
		log.Fatal(err.Error())
	}

	// Set loopback
	config.Host = fmt.Sprintf("%s:6443", host)

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err.Error())
	}

	// Build a options structure to defined what we're looking for
	listOptions := metav1.ListOptions{
		FieldSelector: "metadata.name=kubeadm-config",
	}
	// Watch function
	// Use a restartable watcher, as this should help in the event of etcd or timeout issues
	cluster.rw, err = watchtools.NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return clientset.CoreV1().ConfigMaps(metav1.NamespaceSystem).Watch(listOptions)
		},
	})

	if err != nil {
		return fmt.Errorf("error creating watcher: %s", err.Error())
	}

	ch := cluster.rw.ResultChan()
	//defer cluster.rw.Stop()
	log.Infof("Beginning watching Kubernetes Control Plane Nodes")

	for event := range ch {

		// We need to inspect the event and get ResourceVersion out of it
		switch event.Type {
		case watch.Added, watch.Modified:
			node, ok := event.Object.(*v1.Node)
			if !ok {
				return fmt.Errorf("Unable to parse Node from watcher")
			}

			var nodeAddress, nodeHostname string
			for y := range node.Status.Addresses {
				switch node.Status.Addresses[y].Type {
				case corev1.NodeHostName:
					nodeHostname = node.Status.Addresses[y].Address
				case corev1.NodeInternalIP:
					nodeAddress = node.Status.Addresses[y].Address
				}
			}

			// Retrieve the current configuration and find this server
			var found bool
			for x := range cluster.raftServer.GetConfiguration().Configuration().Servers {
				if cluster.raftServer.GetConfiguration().Configuration().Servers[x].ID == raft.ServerID(nodeHostname) {
					found = true
				}
			}
			if !found {
				log.Infof("New Node [%s] and peer [%s:10000]", nodeHostname, nodeAddress)
				cluster.AddNode(nodeHostname, fmt.Sprintf("%s:10000", nodeAddress))
			}

		case watch.Deleted:
			node, ok := event.Object.(*v1.Node)
			if !ok {
				return fmt.Errorf("Unable to parse Node from watcher")
			}
			var nodeAddress, nodeHostname string
			for y := range node.Status.Addresses {
				switch node.Status.Addresses[y].Type {
				case corev1.NodeHostName:
					nodeHostname = node.Status.Addresses[y].Address
				case corev1.NodeInternalIP:
					nodeAddress = node.Status.Addresses[y].Address
				}
			}
			// Retrieve the current configuration and find this server
			var found bool
			for x := range cluster.raftServer.GetConfiguration().Configuration().Servers {
				if cluster.raftServer.GetConfiguration().Configuration().Servers[x].ID == raft.ServerID(nodeHostname) {
					found = true
				}
			}
			if found {
				log.Infof("Deleting Node [%s] and peer [%s:10000]", nodeHostname, nodeAddress)
				err = cluster.DelNode(nodeHostname)
				log.Errorln(err)
			}

			//return nil
		case watch.Bookmark:
			// Un-used
		case watch.Error:
			log.Infoln("err")

			// This round trip allows us to handle unstructured status
			errObject := apierrors.FromObject(event.Object)
			statusErr, ok := errObject.(*apierrors.StatusError)
			if !ok {
				log.Errorf(spew.Sprintf("Received an error which is not *metav1.Status but %#+v", event.Object))
				// Retry unknown errors
				//return false, 0
			}

			status := statusErr.ErrStatus
			log.Errorf("%v", status)
		default:
		}
	}
	log.Warnln("Stopping watching Kubernetes Control Plane Nodes")

	return nil
}
