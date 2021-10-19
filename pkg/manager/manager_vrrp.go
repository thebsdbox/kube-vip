package manager

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"github.com/kube-vip/kube-vip/pkg/vrrp"
	log "github.com/sirupsen/logrus"
)

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) startVRRP() error {
	var cpCluster *cluster.Cluster
	var err error

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	// ctx, cancel := context.WithCancel(context.Background())
	// defer cancel()

	// use a Go context so we can tell the dns loop code when we
	// want to step down
	ctxDNS, cancelDNS := context.WithCancel(context.Background())
	defer cancelDNS()

	// Shutdown function that will wait on this signal, unless we call it ourselves
	go func() {
		<-sm.signalChan
		log.Info("Received termination, signaling shutdown")
		if sm.config.EnableControlPane {
			cpCluster.Stop()
		}
		// Cancel the context, which will in turn cancel the leadership
		//cancel()
	}()

	if sm.config.EnableControlPane {
		cpCluster, err = cluster.InitCluster(sm.config, false)
		if err != nil {
			return err
		}

		// setup ddns first
		// for first time, need to wait until IP is allocated from DHCP
		if cpCluster.Network.IsDDNS() {
			if err := cpCluster.StartDDNS(ctxDNS); err != nil {
				log.Error(err)
			}
		}

		// start the dns updater if address is dns
		if cpCluster.Network.IsDNS() {
			log.Infof("starting the DNS updater for the address %s", cpCluster.Network.DNSName())
			ipUpdater := vip.NewIPUpdater(cpCluster.Network)
			ipUpdater.Run(ctxDNS)
		}

		// clusterManager := &cluster.Manager{
		// 	KubernetesClient: sm.clientSet,
		// }

		vr := vrrp.NewVirtualRouter(byte(0), sm.config.Interface, false, vrrp.IPv4)
		vr.AddIPvXAddr(net.ParseIP(sm.config.VIP))
		vr.SetPriorityAndMasterAdvInterval(byte(100), time.Millisecond*800)
		vr.Enroll(vrrp.Backup2Master, func() {
			fmt.Println("init to master")
		})
		vr.Enroll(vrrp.Master2Init, func() {
			fmt.Println("master to init")
		})
		vr.Enroll(vrrp.Master2Backup, func() {
			fmt.Println("master to backup")
		})
		// go func() {
		// 	time.Sleep(time.Minute * 5)
		// 	vr.Stop()
		// 	os.Exit(0)
		// }()
		vr.StartWithEventSelector()
		<-sm.signalChan
		vr.Stop()
		log.Infof("Shutting down Kube-Vip")

	}
	return nil
}
