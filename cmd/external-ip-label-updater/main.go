package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
	"github.com/yandex-cloud/go-sdk"
	"github.com/yandex-cloud/go-sdk/iamkey"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

var (
	gitTag, gitCommitId string
)

type patch struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

func main() {
	version := flag.Bool("version", false, "return application version")
	delay := flag.String("delay", "3s", "node ready condition possible delay")
	labelKey := flag.String("label-key", "topology.kubernetes.io/external-ip", "label key which will be store external ip value")
	folderId := flag.String("folder-id", "", "set used Yandex.Cloud folder id")
	saJsonFilePath := flag.String("sa-json-file-path", "", "Yandex.Cloud serviceaccount credentials json file path")
	flag.Parse()

	if *version {
		fmt.Fprintf(os.Stdout, "external-ip-label-updater version: ")
		if gitTag != "" {
			fmt.Fprintf(os.Stdout, "git tag is \"%s\", ", gitTag)
		}
		fmt.Fprintf(os.Stdout, "git commit id is \"%s\"\n", gitCommitId)

		os.Exit(0)
	}

	delayDuration, err := time.ParseDuration(*delay)
	if err != nil {
		log.Fatalf("failed convert delay into time.Duration: %s", err)
	}

	ctx := context.Background()

	// init Yandex.Cloud sdk
	sdk, err := getYandexCloudSDK(ctx, *saJsonFilePath)
	if err != nil {
		log.Fatalf("failed to init Yandex.Cloud sdk: %s", err)
	}

	// init kubernetes informer
	conf, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("failed to get kube connection config: %s", err)
	}

	cli, err := kubernetes.NewForConfig(conf)
	if err != nil {
		log.Fatalf("failed to create kube client: %s", err)
	}

	nodeListWatcher := cache.NewListWatchFromClient(
		cli.CoreV1().RESTClient(), "nodes", v1.NamespaceAll, fields.Everything())
	_, controller := cache.NewInformer(nodeListWatcher, &v1.Node{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if err := updateNodesLabel(ctx, obj, cli, sdk, delayDuration, *folderId, *labelKey); err != nil {
				log.Printf("[error] node label update failed: %s", err)
			}
		},
		UpdateFunc: func(_, obj interface{}) {
			if err := updateNodesLabel(ctx, obj, cli, sdk, delayDuration, *folderId, *labelKey); err != nil {
				log.Printf("[error] node label update failed: %s", err)
			}
		},
		DeleteFunc: func(obj interface{}) {},
	})

	// Now let's start the controller
	stop := make(chan struct{})
	defer close(stop)

	controller.Run(stop)
}

func getYandexCloudSDK(ctx context.Context, path string) (*ycsdk.SDK, error) {
	key, err := iamkey.ReadFromJSONFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to parse iam key file: %s", err)
	}

	credentials, err := ycsdk.ServiceAccountKey(key)
	if err != nil {
		return nil, fmt.Errorf("error credentials create: %s", err)
	}

	sdk, err := ycsdk.Build(ctx, ycsdk.Config{
		Credentials: credentials,
	})
	if err != nil {
		return nil, fmt.Errorf("error sdk build: %s", err)
	}

	return sdk, nil
}

func getYandexCloudComputeInstances(ctx context.Context, sdk *ycsdk.SDK, folderId string) ([]*compute.Instance, error) {
	listInstancesRequest := &compute.ListInstancesRequest{
		FolderId: folderId,
		PageSize: 100,
	}
	resp, err := sdk.Compute().Instance().List(ctx, listInstancesRequest)
	if err != nil {
		return nil, fmt.Errorf("failed to list compule instances: %s", err)
	}

	return resp.Instances, nil
}

func getNodeReadyCondition(node *v1.Node) *v1.NodeCondition {
	for _, condition := range node.Status.Conditions {
		if condition.Type == v1.NodeReady {
			return &condition
		}
	}

	return nil
}

func updateNodesLabel(ctx context.Context, obj interface{}, cli *kubernetes.Clientset, sdk *ycsdk.SDK, delay time.Duration, folderId, label string) error {
	node := obj.(*v1.Node)
	ready := getNodeReadyCondition(node)

	if ready == nil {
		return fmt.Errorf("failed to get node %s ready condition info", node.ObjectMeta.Name)
	}

	if ready.Status == v1.ConditionTrue {
		// if condition changes is only headbeat reason
		// skip proccessing
		if time.Since(ready.LastTransitionTime.Time) > delay {
			log.Printf(
				"[skip] instance %s ready transition time [%s]",
				node.ObjectMeta.Name,
				ready.LastTransitionTime.Time.UTC().Format(time.UnixDate),
			)

			return nil
		}

		instances, err := getYandexCloudComputeInstances(ctx, sdk, folderId)
		if err != nil {
			return fmt.Errorf("failed to get yandex cloud instances list: %s", err)
		}

		for _, instance := range instances {
			if instance.GetName() != node.ObjectMeta.Name {
				continue
			}

			for _, item := range instance.GetNetworkInterfaces() {
				nat := item.GetPrimaryV4Address().GetOneToOneNat()
				if nat != nil {
					ip := nat.GetAddress()

					// skip if label already setted
					if value, ok := node.ObjectMeta.Labels[label]; ok {
						if value == ip {
							log.Printf("[skip] label %s with value %s already exists on instance %s", label, ip, node.ObjectMeta.Name)

							return nil
						}
					}

					log.Printf("[change] adding label %s with value %s to instance %s", label, ip, node.ObjectMeta.Name)

					if err := addNodeLabelWithExternalIP(ctx, cli, node, label, ip); err != nil {
						return fmt.Errorf("failed to add label to node: %s", err)
					}
				}
			}
		}
	} else {
		// skip if label not exists
		if _, ok := node.ObjectMeta.Labels[label]; !ok {
			return nil
		}

		log.Printf("[change] removing label from instance %s", node.ObjectMeta.Name)

		if err := removeNodeLabel(ctx, cli, node, label); err != nil {
			return fmt.Errorf("failed to remove label from node: %s", err)
		}
	}

	return nil
}

func addNodeLabelWithExternalIP(ctx context.Context, cli *kubernetes.Clientset, node *v1.Node, label, address string) error {
	// https://stackoverflow.com/a/52725673
	// escape / to ~1
	label = strings.ReplaceAll(label, "/", "~1")
	label = "/metadata/labels/" + label
	b, err := json.Marshal([]patch{
		{Op: "replace", Path: label, Value: address},
	})
	if err != nil {
		return fmt.Errorf("failed to marshal payload body: %s", err)
	}

	_, err = cli.CoreV1().Nodes().Patch(ctx, node.ObjectMeta.Name, types.JSONPatchType, b, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to patch instance %s: %s", node.ObjectMeta.Name, err)
	}

	return nil
}

func removeNodeLabel(ctx context.Context, cli *kubernetes.Clientset, node *v1.Node, label string) error {
	// https://stackoverflow.com/a/52725673
	// escape / to ~1
	label = strings.ReplaceAll(label, "/", "~1")
	label = "/metadata/labels/" + label
	b, err := json.Marshal([]patch{
		{Op: "remove", Path: label},
	})
	if err != nil {
		return fmt.Errorf("failed to marshal payload body: %s", err)
	}

	_, err = cli.CoreV1().Nodes().Patch(ctx, node.ObjectMeta.Name, types.JSONPatchType, b, metav1.PatchOptions{})

	if err != nil {
		return fmt.Errorf("error in patch request: %s", err)
	}
	if err != nil {
		return fmt.Errorf("failed to patch instance %s: %s", node.ObjectMeta.Name, err)
	}

	return nil
}
