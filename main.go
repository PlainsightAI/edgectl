package main

import (
	"context"
	"fmt"
	"github.com/gofrs/flock"
	"github.com/pkg/errors"
	urcli "github.com/urfave/cli/v2"
	"gopkg.in/yaml.v2"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/cli/values"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/repo"
	"helm.sh/helm/v3/pkg/strvals"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var settings *cli.EnvSettings

var (
	url       = "https://plainsightai.github.io/helm-charts/"
	repoName  = "plainsight-technologies"
	namespace = "plainsight"
)

var (
	version = "unknown"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	app := &urcli.App{
		Version: version,
		Usage:   "Plainsight Edge Controller CLI",
		Commands: []*urcli.Command{
			{
				Name:    "init",
				Aliases: []string{"i"},
				Usage:   "initialize edge device",
				Action:  initAction,
			},
		},
	}
	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func checkHelm() error {
	_, err := exec.LookPath("helm")
	if err != nil {
		println("Helm install docs: https://helm.sh/docs/intro/install/")
		return errors.Wrap(err, "helm not installed or missing from PATH")
	}
	return nil
}

func checkK8s() error {
	_, err := K8sClient()
	if err != nil {
		println("failed to find k8s client config")
		println("make sure ~/.kube/config is setup properly")
		println("to setup k3s run: curl -sfL https://get.k3s.io | sh -")
		println("for more details: https://docs.k3s.io/quick-start")
		return err
	}
	return nil
}

func initAction(cCtx *urcli.Context) error {
	if err := checkK8s(); err != nil {
		return err
	}

	if err := checkHelm(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(cCtx.Context)
	defer cancel()

	k8sClient, err := K8sClient()
	if err != nil {
		println("failed to find k8s client config")
		println("make sure ~/.kube/config is setup properly")
		return err
	}

	_ = os.Setenv("HELM_NAMESPACE", namespace)
	settings = cli.New()
	// Add helm repo
	RepoAdd(repoName, url)
	// Update charts from the helm repo
	RepoUpdate()
	// Setup Plainsight Namaespace
	AddNamespace(ctx, k8sClient)
	// Install NanoMQ chart
	InstallChart("nanomq", repoName, "nanomq", nil)
	return nil
}

func AddNamespace(ctx context.Context, client *kubernetes.Clientset) {
	// check if namespace already exists
	_, err := client.CoreV1().Namespaces().Get(ctx, namespace, v1.GetOptions{})
	if err == nil {
		return
	}

	ns := &corev1.Namespace{ObjectMeta: v1.ObjectMeta{Name: namespace}}
	_, err = client.CoreV1().Namespaces().Create(ctx, ns, v1.CreateOptions{})
	if err != nil {
		log.Fatal(err)
	}
}

// RepoAdd adds repo with given name and url
func RepoAdd(name, url string) {
	repoFile := settings.RepositoryConfig

	//Ensure the file directory exists as it is required for file locking
	err := os.MkdirAll(filepath.Dir(repoFile), os.ModePerm)
	if err != nil && !os.IsExist(err) {
		log.Fatal(err)
	}

	// Acquire a file lock for process synchronization
	fileLock := flock.New(strings.Replace(repoFile, filepath.Ext(repoFile), ".lock", 1))
	lockCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	locked, err := fileLock.TryLockContext(lockCtx, time.Second)
	if err == nil && locked {
		defer func() {
			if err := fileLock.Unlock(); err != nil {
				log.Fatal(err)
			}
		}()
	}
	if err != nil {
		log.Fatal(err)
	}

	b, err := os.ReadFile(repoFile)
	if err != nil && !os.IsNotExist(err) {
		log.Fatal(err)
	}

	var f repo.File
	if err := yaml.Unmarshal(b, &f); err != nil {
		log.Fatal(err)
	}

	if f.Has(name) {
		fmt.Printf("repository name (%s) already exists\n", name)
		return
	}

	c := repo.Entry{Name: name, URL: url}

	r, err := repo.NewChartRepository(&c, getter.All(settings))
	if err != nil {
		log.Fatal(err)
	}

	if _, err := r.DownloadIndexFile(); err != nil {
		err := errors.Wrapf(err, "looks like %q is not a valid chart repository or cannot be reached", url)
		log.Fatal(err)
	}

	f.Update(&c)

	if err := f.WriteFile(repoFile, 0644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%q has been added to your repositories\n", name)
}

// RepoUpdate updates charts for all helm repos
func RepoUpdate() {
	repoFile := settings.RepositoryConfig

	f, err := repo.LoadFile(repoFile)
	if os.IsNotExist(errors.Cause(err)) || len(f.Repositories) == 0 {
		log.Fatal(errors.New("no repositories found. You must add one before updating"))
	}
	var repos []*repo.ChartRepository
	for _, cfg := range f.Repositories {
		r, err := repo.NewChartRepository(cfg, getter.All(settings))
		if err != nil {
			log.Fatal(err)
		}
		repos = append(repos, r)
	}

	fmt.Printf("Hang tight while we grab the latest from your chart repositories...\n")
	var wg sync.WaitGroup
	for _, re := range repos {
		wg.Add(1)
		go func(re *repo.ChartRepository) {
			defer wg.Done()
			if _, err := re.DownloadIndexFile(); err != nil {
				fmt.Printf("...Unable to get an update from the %q chart repository (%s):\n\t%s\n", re.Config.Name, re.Config.URL, err)
			} else {
				fmt.Printf("...Successfully got an update from the %q chart repository\n", re.Config.Name)
			}
		}(re)
	}
	wg.Wait()
	fmt.Printf("Update Complete. ⎈ Happy Helming!⎈\n")
}

// InstallChart
func InstallChart(name, repo, chart string, args map[string]string) {
	actionConfig := new(action.Configuration)
	if err := actionConfig.Init(settings.RESTClientGetter(), settings.Namespace(), os.Getenv("HELM_DRIVER"), debug); err != nil {
		log.Fatal(err)
	}
	client := action.NewInstall(actionConfig)

	if client.Version == "" && client.Devel {
		client.Version = ">0.0.0-0"
	}
	//name, chart, err := client.NameAndChart(args)
	client.ReleaseName = name
	cp, err := client.ChartPathOptions.LocateChart(fmt.Sprintf("%s/%s", repo, chart), settings)
	if err != nil {
		log.Fatal(err)
	}

	debug("CHART PATH: %s\n", cp)

	p := getter.All(settings)
	valueOpts := &values.Options{}
	vals, err := valueOpts.MergeValues(p)
	if err != nil {
		log.Fatal(err)
	}

	// Add args
	if err := strvals.ParseInto(args["set"], vals); err != nil {
		log.Fatal(errors.Wrap(err, "failed parsing --set data"))
	}

	// Check chart dependencies to make sure all are present in /charts
	chartRequested, err := loader.Load(cp)
	if err != nil {
		log.Fatal(err)
	}

	validInstallableChart, err := isChartInstallable(chartRequested)
	if !validInstallableChart {
		log.Fatal(err)
	}

	if req := chartRequested.Metadata.Dependencies; req != nil {
		if err := action.CheckDependencies(chartRequested, req); err != nil {
			if client.DependencyUpdate {
				man := &downloader.Manager{
					Out:              os.Stdout,
					ChartPath:        cp,
					Keyring:          client.ChartPathOptions.Keyring,
					SkipUpdate:       false,
					Getters:          p,
					RepositoryConfig: settings.RepositoryConfig,
					RepositoryCache:  settings.RepositoryCache,
				}
				if err := man.Update(); err != nil {
					log.Fatal(err)
				}
			} else {
				log.Fatal(err)
			}
		}
	}

	client.Namespace = settings.Namespace()
	release, err := client.Run(chartRequested, vals)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(release.Manifest)
}

func isChartInstallable(ch *chart.Chart) (bool, error) {
	switch ch.Metadata.Type {
	case "", "application":
		return true, nil
	}
	return false, errors.Errorf("%s charts are not installable", ch.Metadata.Type)
}

func debug(format string, v ...interface{}) {
	format = fmt.Sprintf("[debug] %s\n", format)
	err := log.Output(2, fmt.Sprintf(format, v...))
	if err != nil {
		return
	}
}

func K8sClient() (*kubernetes.Clientset, error) {

	var (
		c   *rest.Config
		err error
	)

	c, err = rest.InClusterConfig()
	if err != nil {
		// If we err here, let assume we are running outside the cluster
		// use the current context in kubeconfig
		localKubeConfig := filepath.Join(homeDir(), ".kube", "config")
		if _, err := os.Stat(localKubeConfig); errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		c, err = clientcmd.BuildConfigFromFlags("", filepath.Join(homeDir(), ".kube", "config"))
		if err != nil {
			return nil, err
		}
	}

	client, err := kubernetes.NewForConfig(c)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get cluster config")
	}

	return client, nil
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}

	return os.Getenv("USERPROFILE") // windows
}
