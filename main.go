package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	// "io/ioutil"
	"os"
	// "reflect"
	"text/tabwriter"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	compute "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"

	"github.com/yandex-cloud/go-sdk"
	"github.com/yandex-cloud/go-sdk/iamkey"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	// "k8s.io/apimachinery/pkg/util/runtime"
	// "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	// "k8s.io/client-go/util/workqueue"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type patchStringValue struct {
	Op   string `json:"op"`
	Path string `json:"path"`
	// Value string `json:"value"`
	Value interface{} `json:"value"`
}

const (
	duration string = "5m"
)

var (
	credentials ycsdk.Credentials
	sdk         *ycsdk.SDK
	folderID    string

	clientset *kubernetes.Clientset

	timeout time.Duration
)

func init() {
	timeout, _ = time.ParseDuration(duration)

	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	log.SetOutput(os.Stdout)
	log.SetLevel(log.InfoLevel)

	folderID = os.Getenv("YANDEX_CLOUD_FOLDER_ID")

	var key *iamkey.Key = &iamkey.Key{}

	// if your YANDEX_CLOUD_SERVICE_ACCOUNT_JSON env var is a path to file with json
	// use this block rather than simple os.Getenv execution
	// b, err := ioutil.ReadFile(os.Getenv("YANDEX_CLOUD_SERVICE_ACCOUNT_JSON"))
	// if err != nil {
	// 	log.Fatalf("error reading iam key file: %s", err)
	// }

	b := []byte(os.Getenv("YANDEX_CLOUD_SERVICE_ACCOUNT_JSON"))

	err := json.Unmarshal(b, key)
	if err != nil {
		log.Fatalf("error unmarshal iam key file: %s", err)
	}

	credentials, err := ycsdk.ServiceAccountKey(key)
	if err != nil {
		log.Fatalf("error credentials create: %s", err)
	}

	sdk, err = ycsdk.Build(context.TODO(), ycsdk.Config{
		Credentials: credentials,
	})
	if err != nil {
		log.Fatalf("error sdk build: %s", err)
	}
}

func main() {
	var config *rest.Config
	var kubeconfig string = os.Getenv("KUBECONFIG")
	var err error
	var versionFlag bool
	var version, commitID string

	pflag.BoolVar(&versionFlag, "version", false, "return application version")
	pflag.Parse()

	if versionFlag {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		if version != "" {
			fmt.Fprintf(w, "version:\t%s\n", version)
		}
		fmt.Fprintf(w, "git commit:\t%s\n", commitID)
		w.Flush()
		os.Exit(0)
	}

	// creates the connection
	if len(kubeconfig) > 0 {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		log.Fatal(err)
	}

	// creates the clientset
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	// create the pod watcher
	nodeListWatcher := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "nodes", v1.NamespaceAll, fields.Everything())

	// create controller
	_, controller := cache.NewInformer(nodeListWatcher, &v1.Node{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			err := processNode(obj)
			if err != nil {
				log.Errorf("node label update failed: %s", err)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			err := processNode(new)
			if err != nil {
				log.Errorf("node label update failed: %s", err)
			}
		},
		DeleteFunc: func(obj interface{}) {},
	})

	// Now let's start the controller
	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(stop)

	// Wait forever
	select {}
}

func processNode(obj interface{}) error {
	node := obj.(*v1.Node)

	if len(node.Status.Conditions) > 0 {
		now := time.Now().Unix()
		lastCondition := node.Status.Conditions[len(node.Status.Conditions)-1]

		if (now-lastCondition.LastTransitionTime.Unix()) < int64(timeout.Seconds()) &&
			lastCondition.Type == v1.NodeReady {
			if lastCondition.Status == v1.ConditionTrue {
				err := addLabel(node.ObjectMeta.Name)
				if err != nil {
					return err
				}
			} else {
				err := delLabel(node.ObjectMeta.Name)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func delLabel(nodeName string) error {
	node, err := clientset.CoreV1().Nodes().Get(
		context.TODO(),
		nodeName,
		metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("error getting node: %s", err)
	}

	if _, ok := node.ObjectMeta.Labels["topology.kubernetes.io/external-ip"]; ok {
		log.Infof("removing label from %s node", nodeName)

		payload := []patchStringValue{{
			Op: "remove",
			// https://stackoverflow.com/a/52725673
			// escape / to ~1
			Path: "/metadata/labels/topology.kubernetes.io~1external-ip",
		}}

		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("error in marshaling patch body: %s", err)
		}

		_, err = clientset.CoreV1().Nodes().Patch(
			// resp, err := clientset.CoreV1().Nodes().Patch(
			context.TODO(),
			nodeName,
			types.JSONPatchType,
			// types.StrategicMergePatchType,
			payloadBytes,
			metav1.PatchOptions{},
		)

		if err != nil {
			return fmt.Errorf("error in patch request: %s", err)
		}

		log.Infof("patched node %s. Label topology.kubernetes.io/external-ip removed", nodeName)
	}

	return nil
}

func addLabel(nodeName string) error {
	log.Infof("adding label to %s node", nodeName)

	instances, err := getComputeInstances()
	if err != nil {
		log.Errorf("error instance list get: %s", err)
	}

	for _, instance := range instances {
		var externalIP string

		if len(instance.NetworkInterfaces) > 0 {
			externalIP = instance.NetworkInterfaces[0].PrimaryV4Address.OneToOneNat.Address
		}

		if instanceName := instance.Name; nodeName == instanceName && externalIP != "" {
			payload := []patchStringValue{{
				Op: "replace",
				// https://stackoverflow.com/a/52725673
				// escape / to ~1
				Path:  "/metadata/labels/topology.kubernetes.io~1external-ip",
				Value: externalIP,
			}}

			payloadBytes, err := json.Marshal(payload)
			if err != nil {
				return fmt.Errorf("error in marshaling patch body: %s", err)
			}

			_, err = clientset.CoreV1().Nodes().Patch(
				context.TODO(),
				nodeName,
				types.JSONPatchType,
				payloadBytes,
				metav1.PatchOptions{},
			)

			if err != nil {
				return fmt.Errorf("error in patch request: %s", err)
			}

			log.Infof("patched node %s with label topology.kubernetes.io/external-ip=%s", nodeName, externalIP)
		}
	}

	return nil
}

func getComputeInstances() ([]*compute.Instance, error) {
	resp, err := sdk.Compute().Instance().List(
		context.TODO(),
		&compute.ListInstancesRequest{
			FolderId: folderID,
			PageSize: 50,
		},
	)
	if err != nil {
		return nil, err
	}

	return resp.Instances, nil
}
