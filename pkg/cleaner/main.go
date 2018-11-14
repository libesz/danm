package main

import (
	"flag"
	"os"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/golang/glog"
	danmtypes "github.com/nokia/danm/pkg/crd/apis/danm/v1"
	danmclientset "github.com/nokia/danm/pkg/crd/client/clientset/versioned"
	"github.com/nokia/danm/pkg/danmep"
	"github.com/nokia/danm/pkg/ipam"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	kubeconfig string
)

//////////////////
// EventHandler //
//////////////////
type EventHandler struct {
	cli *danmclientset.Clientset
}

func (handler *EventHandler) ProcessEvent(event *docker.APIEvents) {
	containerAttrs := event.Actor.Attributes
	// only pause is important as it has the networking
	if containerAttrs["io.kubernetes.docker.type"] != "podsandbox" {
		return
	}

	// events which can cause problems: kill, die, stop, destroy
	// they are coming even under normal operation but before that cni deletes the danmeps
	if event.Action == "kill" || event.Action == "die" || event.Action == "stop" || event.Action == "destroy" {
		eps, err := danmep.FindByCid(handler.cli, event.ID)
		if err != nil {
			return
		}
		glog.Infof("eps: %v", eps)
		for _, ep := range eps {
			deleteInterface(handler.cli, ep)
		}
	}
}

func (handler *EventHandler) Watch(client *docker.Client) {
	listener := make(chan *docker.APIEvents)
	err := client.AddEventListener(listener)
	if err != nil {
		glog.Fatal(err)
	}
	glog.Info("Connected")

	defer func() {

		err = client.RemoveEventListener(listener)
		if err != nil {
			glog.Fatal(err)
		}

	}()

	for {
		select {
		case msg := <-listener:
			if msg.Type == "container" {
				handler.ProcessEvent(msg)
			}
		}
	}
}

///////////////
// functions //
///////////////

///////////////////////////////////////////////////////
// cleanup removes host specific endpoints which are //
// belonging to dead containers on the host          //
// or exists in k8s only but not on the host         //
///////////////////////////////////////////////////////
func cleanup(dockercli *docker.Client, danmcli *danmclientset.Clientset) {
	// eps on host according to k8s
	eps, err := danmep.CidsByHost(danmcli, getHostname())
	if err != nil {
		glog.Errorf("Cleanup failed: %v", err)
		return
	}
	optDead := docker.ListContainersOptions{All: true, Filters: map[string][]string{"status": {"exited"}}}
	optAll := docker.ListContainersOptions{All: true}
	// exited containers on the host according to host
	dockerDeads, err := dockercli.ListContainers(optDead)
	if err != nil {
		glog.Errorf("Cleanup failed: %v", err)
		return
	}
	// all containers on the host according to host
	dockerAll, err := dockercli.ListContainers(optAll)
	if err != nil {
		glog.Errorf("Cleanup failed: %v", err)
		return
	}
	var cids []string
	// all container ids on the hosts according to the host
	for _, container := range dockerAll {
		cids = append(cids, container.ID)
	}
	// delete dead containers' eps from k8s
	for _, dead := range dockerDeads {
		for _, ep := range eps {
			if ep.Spec.CID == dead.ID {
				deleteInterface(danmcli, ep)
			}
		}
	}
	// delete eps from k8s which are not existing on the host
	for _, ep := range eps {
		if !contains(cids, ep.Spec.CID) {
			deleteInterface(danmcli, ep)
		}
	}
}

/////////////////////////////////////////
// check wether array contains element //
/////////////////////////////////////////
func contains(x []string, y string) bool {
	for _, z := range x {
		if z == y {
			return true
		}
	}
	return false
}

//////////////////////////////////////////////////////
// deletes ep from danmnet bitarray and from danmep //
//////////////////////////////////////////////////////
func deleteInterface(cli *danmclientset.Clientset, ep danmtypes.DanmEp) {
	netInfo, err := cli.DanmV1().DanmNets(ep.ObjectMeta.Namespace).Get(ep.Spec.NetworkID, meta_v1.GetOptions{})
	if err != nil {
		glog.Errorf("Cannot fetch net info for net: %s", ep.Spec.NetworkID)
		return
	}
	// For ipvlan where cidr is defined free up reserved ip address
	if ep.Spec.NetworkType != "ipvlan" {
		ipam.Free(cli, *netInfo, ep.Spec.Iface.Address)
	}
	// delete danmep crd from apiserver
	cli.DanmV1().DanmEps(ep.ObjectMeta.Namespace).Delete(ep.ObjectMeta.Name, &meta_v1.DeleteOptions{})
}

///////////////////////////
// returns with hostname //
///////////////////////////
func getHostname() string {
	ret, err := os.Hostname()
	if err != nil {
		glog.Fatalf("hostname %v", err)
	}
	return ret
}

/////////////////////////////////
// creates k8s client for danm //
/////////////////////////////////
func danmClient() *danmclientset.Clientset {
	flag.Parse()
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		glog.Fatalf("Error building kubeconfig: %s", err.Error())
	}
	danmClient, err := danmclientset.NewForConfig(cfg)
	if err != nil {
		glog.Fatalf("Error building example clientset: %s", err.Error())
	}

	return danmClient
}

func main() {
	client, err := docker.NewClientFromEnv()
	if err != nil {
		panic(err)
	}
	cli := danmClient()
	cleanup(client, cli)
	eh := &EventHandler{cli: cli}
	eh.Watch(client)
}

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
}
