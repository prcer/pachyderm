package cmd

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/apis/batch"

	"github.com/gogo/protobuf/types"
	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/version"
	"github.com/pachyderm/pachyderm/src/client/version/versionpb"
	pfscmds "github.com/pachyderm/pachyderm/src/server/pfs/cmds"
	"github.com/pachyderm/pachyderm/src/server/pkg/cmdutil"
	deploycmds "github.com/pachyderm/pachyderm/src/server/pkg/deploy/cmds"
	"github.com/pachyderm/pachyderm/src/server/pkg/metrics"
	ppscmds "github.com/pachyderm/pachyderm/src/server/pps/cmds"

	log "github.com/Sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/ugorji/go/codec"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"
)

// PachctlCmd takes a pachd host-address and creates a cobra.Command
// which may interact with the host.
func PachctlCmd(address string) (*cobra.Command, error) {
	var verbose bool
	var noMetrics bool
	rootCmd := &cobra.Command{
		Use: os.Args[0],
		Long: `Access the Pachyderm API.

Environment variables:
  ADDRESS=<host>:<port>, the pachd server to connect to (e.g. 127.0.0.1:30650).
`,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if !verbose {
				// Silence any grpc logs
				l := log.New()
				l.Level = log.FatalLevel
				grpclog.SetLogger(l)
			}
		},
	}
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Output verbose logs")
	rootCmd.PersistentFlags().BoolVarP(&noMetrics, "no-metrics", "", false, "Don't report user metrics for this command")

	pfsCmds := pfscmds.Cmds(address, &noMetrics)
	for _, cmd := range pfsCmds {
		rootCmd.AddCommand(cmd)
	}
	ppsCmds, err := ppscmds.Cmds(address, &noMetrics)
	if err != nil {
		return nil, sanitizeErr(err)
	}
	for _, cmd := range ppsCmds {
		rootCmd.AddCommand(cmd)
	}
	deployCmds := deploycmds.Cmds(&noMetrics)
	for _, cmd := range deployCmds {
		rootCmd.AddCommand(cmd)
	}

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Return version information.",
		Long:  "Return version information.",
		Run: cmdutil.RunFixedArgs(0, func(args []string) (retErr error) {
			if !noMetrics {
				start := time.Now()
				startMetricsWait := metrics.StartReportAndFlushUserAction("Version", start)
				defer startMetricsWait()
				defer func() {
					finishMetricsWait := metrics.FinishReportAndFlushUserAction("Version", retErr, start)
					finishMetricsWait()
				}()
			}
			writer := tabwriter.NewWriter(os.Stdout, 20, 1, 3, ' ', 0)
			printVersionHeader(writer)
			printVersion(writer, "pachctl", version.Version)
			writer.Flush()

			versionClient, err := getVersionAPIClient(address)
			if err != nil {
				return sanitizeErr(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			version, err := versionClient.GetVersion(ctx, &types.Empty{})

			if err != nil {
				buf := bytes.NewBufferString("")
				errWriter := tabwriter.NewWriter(buf, 20, 1, 3, ' ', 0)
				fmt.Fprintf(errWriter, "pachd\t(version unknown) : error connecting to pachd server at address (%v): %v\n\nplease make sure pachd is up (`kubectl get all`) and portforwarding is enabled\n", address, sanitizeErr(err))
				errWriter.Flush()
				return errors.New(buf.String())
			}

			printVersion(writer, "pachd", version)
			return writer.Flush()
		}),
	}
	deleteAll := &cobra.Command{
		Use:   "delete-all",
		Short: "Delete everything.",
		Long: `Delete all repos, commits, files, pipelines and jobs.
This resets the cluster to its initial state.`,
		Run: cmdutil.RunFixedArgs(0, func(args []string) error {
			client, err := client.NewMetricsClientFromAddress(address, !noMetrics, "user")
			if err != nil {
				return sanitizeErr(err)
			}
			fmt.Printf("Are you sure you want to delete all repos, commits, files, pipelines and jobs? yN\n")
			r := bufio.NewReader(os.Stdin)
			bytes, err := r.ReadBytes('\n')
			if err != nil {
				return err
			}
			if bytes[0] == 'y' || bytes[0] == 'Y' {
				return client.DeleteAll()
			}
			return nil
		}),
	}
	var port int
	var uiPort int
	var uiWebsocketPort int
	var kubeCtlFlags string
	portForward := &cobra.Command{
		Use:   "port-forward",
		Short: "Forward a port on the local machine to pachd. This command blocks.",
		Long:  "Forward a port on the local machine to pachd. This command blocks.",
		Run: cmdutil.RunFixedArgs(0, func(args []string) error {

			var eg errgroup.Group

			eg.Go(func() error {
				stdin := strings.NewReader(fmt.Sprintf(`
pod=$(kubectl %v get pod -l app=pachd | awk '{if (NR!=1) { print $1; exit 0 }}')
kubectl %v port-forward "$pod" %d:650
`, kubeCtlFlags, kubeCtlFlags, port))
				return cmdutil.RunIO(cmdutil.IO{
					Stdin:  stdin,
					Stderr: os.Stderr,
				}, "sh")
			})

			eg.Go(func() error {
				stdin := strings.NewReader(fmt.Sprintf(`
pod=$(kubectl %v get pod -l app=dash | awk '{if (NR!=1) { print $1; exit 0 }}')
kubectl %v port-forward "$pod" %d:8080
`, kubeCtlFlags, kubeCtlFlags, uiPort))
				if err := cmdutil.RunIO(cmdutil.IO{
					Stdin: stdin,
				}, "sh"); err != nil {
					return fmt.Errorf("UI not enabled, deploy with --dashboard")
				}
				return nil
			})

			eg.Go(func() error {
				stdin := strings.NewReader(fmt.Sprintf(`
pod=$(kubectl %v get pod -l app=dash | awk '{if (NR!=1) { print $1; exit 0 }}')
kubectl %v port-forward "$pod" %d:8081
`, kubeCtlFlags, kubeCtlFlags, uiWebsocketPort))
				cmdutil.RunIO(cmdutil.IO{
					Stdin: stdin,
				}, "sh")
				return nil
			})

			fmt.Printf("Pachd port forwarded\nDash websocket port forwarded\nDash UI port forwarded, navigate to localhost:%v\nCTRL-C to exit", uiPort)
			return eg.Wait()
		}),
	}
	portForward.Flags().IntVarP(&port, "port", "p", 30650, "The local port to bind to.")
	portForward.Flags().IntVarP(&uiPort, "ui-port", "u", 38080, "The local port to bind to.")
	portForward.Flags().IntVarP(&uiWebsocketPort, "proxy-port", "x", 38081, "The local port to bind to.")
	portForward.Flags().StringVarP(&kubeCtlFlags, "kubectlflags", "k", "", "Any kubectl flags to proxy, e.g. --kubectlflags='--kubeconfig /some/path/kubeconfig'")

	garbageCollect := &cobra.Command{
		Use:   "garbage-collect",
		Short: "Garbage collect unused data.",
		Long: `Garbage collect unused data.

When a file/commit/repo is deleted, the data is not immediately removed from the underlying storage system (e.g. S3) for performance and architectural reasons.  This is similar to how when you delete a file on your computer, the file is not necessarily wiped from disk immediately.

To actually remove the data, you will need to manually invoke garbage collection.  The easiest way to do it is through "pachctl garbage-collecth".

Currently "pachctl garbage-collect" can only be started when there are no active jobs running.  You also need to ensure that there's no ongoing "put-file".  Garbage collection puts the cluster into a readonly mode where no new jobs can be created and no data can be added.
`,
		Run: cmdutil.RunFixedArgs(0, func(args []string) (retErr error) {
			client, err := client.NewMetricsClientFromAddress(address, !noMetrics, "user")
			if err != nil {
				return err
			}

			return client.GarbageCollect()
		}),
	}

	var from, to, namespace string
	migrate := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate the internal state of Pachyderm from one version to another.",
		Long: `Migrate the internal state of Pachyderm from one version to
another.  Note that most of the time updating Pachyderm doesn't
require a migration.  Refer to the docs for your specific version
to find out if it requires a migration.

It's highly recommended that you only run migrations when there are no
activities in your cluster, e.g. no jobs should be running.

The migration command takes the general form:

$ pachctl migrate --from <FROM_VERSION> --to <TO_VERSION>

If "--from" is not provided, pachctl will attempt to discover the current
version of the cluster.  If "--to" is not provided, pachctl will use the
version of pachctl itself.

Example:

# Migrate Pachyderm from 1.4.8 to 1.5.0
$ pachctl migrate --from 1.4.8 --to 1.5.0
`,
		Run: cmdutil.RunFixedArgs(0, func(args []string) (retErr error) {
			// If `from` is not provided, we use the cluster version.
			if from == "" {
				versionClient, err := getVersionAPIClient(address)
				if err != nil {
					return sanitizeErr(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				clusterVersion, err := versionClient.GetVersion(ctx, &types.Empty{})
				if err != nil {
					return fmt.Errorf("unable to discover cluster version; please provide the --from flag.  Error: %v", err)
				}
				from = version.PrettyPrintVersionNoAdditional(clusterVersion)
			}

			// if `to` is not provided, we use the version of pachctl itself.
			if to == "" {
				to = version.PrettyPrintVersionNoAdditional(version.Version)
			}

			jobSpec := batch.Job{
				TypeMeta: unversioned.TypeMeta{
					Kind:       "Job",
					APIVersion: "batch/v1",
				},
				ObjectMeta: api.ObjectMeta{
					Name: "pach-migration",
					Labels: map[string]string{
						"suite": "pachyderm",
					},
				},
				Spec: batch.JobSpec{
					Template: api.PodTemplateSpec{
						Spec: api.PodSpec{
							Containers: []api.Container{
								{
									Name:    "migration",
									Image:   fmt.Sprintf("pachyderm/pachd:%v", version.PrettyPrintVersion(version.Version)),
									Command: []string{"/pachd", fmt.Sprintf("--migrate=%v-%v", from, to)},
								},
							},
							RestartPolicy: "OnFailure",
						},
					},
				},
			}

			tmpFile, err := ioutil.TempFile("", "")
			if err != nil {
				return err
			}
			defer os.Remove(tmpFile.Name())

			jsonEncoderHandle := &codec.JsonHandle{
				BasicHandle: codec.BasicHandle{
					EncodeOptions: codec.EncodeOptions{Canonical: true},
				},
				Indent: 2,
			}
			encoder := codec.NewEncoder(tmpFile, jsonEncoderHandle)
			jobSpec.CodecEncodeSelf(encoder)
			tmpFile.Close()

			cmd := exec.Command("kubectl", "create", "--validate=false", "-f", tmpFile.Name())
			out, err := cmd.CombinedOutput()
			fmt.Println(string(out))
			if err != nil {
				return err
			}
			fmt.Println("Successfully launched migration.  To see the progress, use `kubectl logs job/pach-migration`")
			return nil
		}),
	}
	migrate.Flags().StringVar(&from, "from", "", "The current version of the cluster.  If not specified, pachctl will attempt to discover the version of the cluster.")
	migrate.Flags().StringVar(&to, "to", "", "The version of Pachyderm to migrate to.  If not specified, pachctl will use its own version.")
	migrate.Flags().StringVar(&namespace, "namespace", "default", "The kubernetes namespace under which Pachyderm is deployed.")

	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(deleteAll)
	rootCmd.AddCommand(portForward)
	rootCmd.AddCommand(garbageCollect)
	rootCmd.AddCommand(migrate)
	return rootCmd, nil
}

func getVersionAPIClient(address string) (versionpb.APIClient, error) {
	clientConn, err := grpc.Dial(address, client.PachDialOptions()...)
	if err != nil {
		return nil, err
	}
	return versionpb.NewAPIClient(clientConn), nil
}

func printVersionHeader(w io.Writer) {
	fmt.Fprintf(w, "COMPONENT\tVERSION\t\n")
}

func printVersion(w io.Writer, component string, v *versionpb.Version) {
	fmt.Fprintf(w, "%s\t%s\t\n", component, version.PrettyPrintVersion(v))
}

func sanitizeErr(err error) error {
	if err == nil {
		return nil
	}

	return errors.New(grpc.ErrorDesc(err))
}
