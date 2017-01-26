package main //import "github.com/nordstrom/prometheusRuleLoader"

import (
	"bufio"
	"crypto/sha1"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"

	//"gopkg.in/yaml.v2"

	kapi "k8s.io/kubernetes/pkg/api"
	kcache "k8s.io/kubernetes/pkg/client/cache"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	kframework "k8s.io/kubernetes/pkg/controller/framework"
	kselector "k8s.io/kubernetes/pkg/fields"
	klabels "k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/util/wait"
)

var (
	// FLAGS
	configmapAnnotation = flag.String("annotation", os.Getenv("CONFIG_MAP_ANNOTATION"), "Annotation that states that this configmap contains prometheus rules.")
	rulesLocation       = flag.String("rulespath", os.Getenv("RULES_LOCATION"), "Filepath where the rules from the configmap file should be written, this should correspond to a rule_files: location in your prometheus config.")
	reloadEndpoint      = flag.String("endpoint", os.Getenv("PROMETHEUS_RELOAD_ENDPOINT"), "Endpoint of the Prometheus reset endpoint (eg: http://prometheus:9090/-/reload).")

	helpFlag      = flag.Bool("help", false, "")
	lastSvcSha    = ""
	testRule      = "[ \"ALERT helloworldHealthCounter IF sum(helloWorldHealthCounter) == 0 FOR 1m LABELS { severity = 'critical' } ANNOTATIONS { description = 'hello-world is down.' }\", \"ALERT itemqueryserviceHealthCounter IF sum(helloWorldHealthCounter) == 0 FOR 1m LABELS { severity = 'critical' } ANNOTATIONS { description = 'item-query-service is down.' }\", \"ALERT pointofserviceHealthCounter IF sum(helloWorldHealthCounter) == 0 FOR 1m LABELS { severity = 'critical' } ANNOTATIONS { description = 'point-of-service is down.' }\" ]"
	annotationKey = flag.String("annotationKey", "nordstrom.net/prometheusAlerts", "Annotation key for prometheus rules")
)

const (
	// Resync period for the kube controller loop.
	resyncPeriod = 30 * time.Minute
	// A subdomain added to the user specified domain for all services.
	serviceSubdomain = "svc"
	// A subdomain added to the user specified dmoain for all pods.
	podSubdomain = "pod"
)

func main() {
	flag.Parse()

	if *helpFlag ||
		*configmapAnnotation == "" ||
		*rulesLocation == "" ||
		*reloadEndpoint == "" {
		flag.PrintDefaults()
		os.Exit(0)
	}

	log.Printf("Rule Updater loaded.\n")
	log.Printf("ConfigMap annotation: %s\n", *configmapAnnotation)
	log.Printf("Rules location: %s\n", *rulesLocation)

	// create client
	var kubeClient *kclient.Client
	kubeClient, err := kclient.NewInCluster()
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}

	// load base config
	// I think this is uneeded
	//updateRules(kubeClient, *rulesLocation)
	//reloadRules(*reloadEndpoint)

	// setup file watcher, will trigger whenever the configmap updates

	// setup watcher for configmaps coming and going
	_ = watchForConfigmaps(kubeClient, func(interface{}) {
		log.Printf("Configmaps have updated.\n")
		check := updateRules(kubeClient, *rulesLocation)
		if check {
			err := reloadRules(*reloadEndpoint)
			if err != nil {
				log.Println(err)
			}
		}
	})

	defer func() {
		log.Printf("Cleaning up.")
	}()

	select {}
}

func updateRules(kubeClient *kclient.Client, rulesLocation string) bool {
	log.Println("Processing rules.")

	ruleList := GatherRulesFromConfigmaps(kubeClient)

	var rulesToWrite string
	for _, rule := range ruleList {
		content, err := processRuleString(rule)
		if err != nil {
			log.Printf("%s", err)
		} else {
			rulesToWrite += fmt.Sprintf("%s\n", content)
		}
	}

	err := CheckRules(rulesToWrite)
	if err != nil {
		log.Printf("Generated rule does not pass: %s.\n%s\n", err, rulesToWrite)
	}

	// only write and
	newSha := computeSha1(rulesToWrite)
	if lastSvcSha != newSha {
		err = writeRules(rulesToWrite, rulesLocation)
		if err != nil {
			log.Printf("%s\n", err)
		}
		lastSvcSha = newSha
		return true
	}
	log.Println("No changes, skipping write.")
	return false
}

func GatherRulesFromConfigmaps(kubeClient *kclient.Client) []string {
	si := kubeClient.ConfigMaps(kapi.NamespaceAll)
	mapList, err := si.List(kapi.ListOptions{
		LabelSelector: klabels.Everything(),
		FieldSelector: kselector.Everything()})
	if err != nil {
		log.Printf("Unable to list configmaps: %s", err)
	}

	ruleList := []string{}

	for _, cm := range mapList.Items {
		anno := cm.GetObjectMeta().GetAnnotations()
		name := cm.GetObjectMeta().GetName()

		for k := range anno {
			if k == *configmapAnnotation {
				log.Printf("Annotation found, processing Configmap - %s\n", name)
				for cmk, cmv := range cm.Data {
					log.Printf("Found potential rule - %s\n", cmk)
					ruleList = append(ruleList, cmv)
				}
			}
		}

	}

	return ruleList
}

func writeRules(rules string, rulesLocation string) error {
	f, err := os.Create(rulesLocation)
	if err != nil {
		return fmt.Errorf("Unable to open rules file %s for writing. Error: %s", rulesLocation, err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	byteCount, err := w.WriteString(rules)
	if err != nil {
		return fmt.Errorf("Unable to write generated rules. Error: %s", err)
	}
	log.Printf("Wrote %d bytes.\n", byteCount)
	w.Flush()

	return nil
}

func processRuleString(rule string) (string, error) {
	err := CheckRules(rule)
	if err != nil {
		return "", fmt.Errorf("Rule rejected, Error: %s\n, Rule: %s", err, rule)
	}
	log.Printf("Rule passed!\n")

	return rule, nil
}

func reloadRules(url string) error {
	client := &http.Client{}
	req, err := http.NewRequest("POST", url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Unable to reload Prometheus config: %s", err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		log.Printf("Prometheus configuration reloaded.")
		return nil
	}

	respBody, _ := ioutil.ReadAll(resp.Body)
	return fmt.Errorf("Unable to reload the Prometheus config. Endpoint: %s, Reponse StatusCode: %d, Response Body: %s", url, resp.StatusCode, string(respBody))
}

func createConfigmapLW(kubeClient *kclient.Client) *kcache.ListWatch {
	return kcache.NewListWatchFromClient(kubeClient, "configmaps", kapi.NamespaceAll, kselector.Everything())
}

func watchForConfigmaps(kubeClient *kclient.Client, callback func(interface{})) kcache.Store {
	configmapStore, configmapController := kframework.NewInformer(
		createConfigmapLW(kubeClient),
		&kapi.ConfigMap{},
		0,
		kframework.ResourceEventHandlerFuncs{
			AddFunc:    callback,
			DeleteFunc: callback,
			UpdateFunc: func(a interface{}, b interface{}) { callback(b) },
		},
	)
	go configmapController.Run(wait.NeverStop)
	return configmapStore
}

func computeSha1(payload string) string {
	hash := sha1.New()
	hash.Write([]byte(payload))

	return fmt.Sprintf("%x", hash.Sum(nil))
}
