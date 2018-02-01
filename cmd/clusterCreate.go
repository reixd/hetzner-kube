// Copyright © 2018 NAME HERE <EMAIL ADDRESS>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"github.com/spf13/cobra"
	"log"
	"errors"
	"fmt"
	"github.com/hetznercloud/hcloud-go/hcloud"
	"strings"
	"time"
	"github.com/xetys/hetzner-kube/pkg"
	"github.com/Pallinder/go-randomdata"
)

// clusterCreateCmd represents the clusterCreate command
var clusterCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "creates a cluster",
	Long: `A longer description that spans multiple lines and likely contains examples
and usage of using your command. For example:

Cobra is a CLI library for Go that empowers applications.
This application is a tool to generate the needed files
to quickly create a Cobra application.`,
	PreRunE: validateClusterCreateFlags,
	Run: func(cmd *cobra.Command, args []string) {

		nodeCount, _ := cmd.Flags().GetInt("nodes")
		workerCount := nodeCount - 1

		clusterName := randomName()
		if name, _ := cmd.Flags().GetString("name"); name != "" {
			clusterName = name
		}

		sshKeyName, _ := cmd.Flags().GetString("ssh-key")
		masterServerType, _ := cmd.Flags().GetString("master-server-type")
		workerServerType, _ := cmd.Flags().GetString("worker-server-type")

		cluster := Cluster{Name: clusterName, wait: false}

		if err := cluster.CreateMasterNodes(Node{SSHKeyName: sshKeyName, IsMaster: true, Type: masterServerType}, 1); err != nil {
			log.Println(err)
		}

		saveCluster(&cluster)

		if workerCount > 0 {
			if err := cluster.CreateWorkerNodes(Node{SSHKeyName: sshKeyName, IsMaster: false, Type: workerServerType}, workerCount); err != nil {
				log.Fatal(err)
			}
		}
		saveCluster(&cluster)

		if cluster.wait {
			log.Println("sleep for 30s...")
			time.Sleep(30 * time.Second)
		}
		cluster.coordinator = pkg.NewProgressCoordinator()
		cluster.RenderProgressBars()

		// provision nodes
		tries := 0
		for err := cluster.ProvisionNodes(); err != nil; {
			if tries < 3 {
				fmt.Print(err)
				tries++
			} else {
				log.Fatal(err)
			}
		}

		// install master
		if err := cluster.InstallMaster(); err != nil {
			log.Fatal(err)
		}

		saveCluster(&cluster)

		// install worker
		if err := cluster.InstallWorkers(); err != nil {
			log.Fatal(err)
		}

		cluster.coordinator.Wait()
		log.Println("Cluster successfully created!")

		saveCluster(&cluster)
	},
}

func saveCluster(cluster *Cluster) {
	AppConf.Config.AddCluster(*cluster)
	AppConf.Config.WriteCurrentConfig()
}

func (cluster *Cluster) CreateNodes(suffix string, template Node, count int) error {
	sshKey, _, err := AppConf.Client.SSHKey.Get(AppConf.Context, template.SSHKeyName)

	if err != nil {
		return err
	}

	serverNameTemplate := fmt.Sprintf("%s-%s-@idx", cluster.Name, suffix)
	serverOptsTemplate := hcloud.ServerCreateOpts{
		Name: serverNameTemplate,
		ServerType: &hcloud.ServerType{
			Name: template.Type,
		},
		Image: &hcloud.Image{
			Name: "ubuntu-16.04",
		},
	}

	serverOptsTemplate.SSHKeys = append(serverOptsTemplate.SSHKeys, sshKey)

	for i := 1; i <= count; i++ {
		var serverOpts hcloud.ServerCreateOpts
		serverOpts = serverOptsTemplate
		serverOpts.Name = strings.Replace(serverNameTemplate, "@idx", fmt.Sprintf("%.02d", i), 1)

		// create
		server, err := cluster.runCreateServer(&serverOpts)

		if err != nil {
			return err
		}

		ipAddress := server.Server.PublicNet.IPv4.IP.String()
		log.Printf("Created node '%s' with IP %s", server.Server.Name, ipAddress)
		cluster.Nodes = append(cluster.Nodes, Node{
			Name:       serverOpts.Name,
			Type:       serverOpts.ServerType.Name,
			IsMaster:   template.IsMaster,
			IPAddress:  ipAddress,
			SSHKeyName: template.SSHKeyName,
		})
	}

	return nil
}

func (cluster *Cluster) runCreateServer(opts *hcloud.ServerCreateOpts) (*hcloud.ServerCreateResult, error) {

	log.Printf("creating server '%s'...", opts.Name)
	result, _, err := AppConf.Client.Server.Create(AppConf.Context, *opts)
	if err != nil {
		if err.(hcloud.Error).Code == "uniqueness_error" {
			server, _, err := AppConf.Client.Server.Get(AppConf.Context, opts.Name)

			if err != nil {
				return nil, err
			}

			log.Printf("loading server '%s'...", opts.Name)
			return &hcloud.ServerCreateResult{Server: server}, nil
		}

		return nil, err
	}

	if err := AppConf.ActionProgress(AppConf.Context, result.Action); err != nil {
		return nil, err
	}

	cluster.wait = true

	return &result, nil
}

func (cluster *Cluster) CreateMasterNodes(template Node, count int) error {
	log.Println("creating master nodes...")
	return cluster.CreateNodes("master", template, count)
}

func (cluster *Cluster) CreateWorkerNodes(template Node, count int) error {
	return cluster.CreateNodes("worker", template, count)
}

func (cluster *Cluster) RenderProgressBars() {
	for _, node := range cluster.Nodes {
		steps := 0
		if node.IsMaster {
			// the InstallMaster routine has 9 events
			steps += 9

			// and one more, it's got tainted
			if len(cluster.Nodes) == 1 {
				steps += 1
			}
		} else {
			steps = 4
		}

		cluster.coordinator.StartProgress(node.Name, steps)
	}
}

func (cluster *Cluster) ProvisionNodes() error {
	processes := 0
	//c := make(chan int)
	//ce := make(chan error)
	for _, node := range cluster.Nodes {
		// log.Printf("installing docker.io and kubeadm on node '%s'...", node.Name)
		processes++
		// go func() {
		// node := node
		cluster.coordinator.AddEvent(node.Name, fmt.Sprintf("install packages on %s", node.IPAddress))
		_, err := runCmd(node, "wget -cO- https://raw.githubusercontent.com/xetys/hetzner-kube/master/install-docker-kubeadm.sh | bash -")

		//if err != nil {
		//	ce <- err
		//} else {
		//	c <- 1
		//}

		if err != nil {
			return err
		}

		if node.IsMaster {
			cluster.coordinator.AddEvent(node.Name, "packages installed")
		} else {
			cluster.coordinator.AddEvent(node.Name, "waiting for master")
		}


		//}()
	}

	//for processes > 0 {
	//	select {
	//	case err := <-ce:
	//		return err
	//	case <-c:
	//		processes--
	//	}
	//}

	return nil
}
func (cluster *Cluster) InstallMaster() error {
	commands := []SSHCommand{
		{"disable swap", "swapoff -a"},
		{"kubeadm init", "kubeadm reset && kubeadm init --pod-network-cidr=10.244.0.0/16"},
		{"configure kubectl", "mkdir -p $HOME/.kube && cp -i /etc/kubernetes/admin.conf $HOME/.kube/config && chown $(id -u):$(id -g) $HOME/.kube/config"},
		{"install flannel", "kubectl apply -f https://raw.githubusercontent.com/coreos/flannel/v0.9.1/Documentation/kube-flannel.yml"},
		{"configure flannel", "kubectl -n kube-system patch ds kube-flannel-ds --type json -p '[{\"op\":\"add\",\"path\":\"/spec/template/spec/tolerations/-\",\"value\":{\"key\":\"node.cloudprovider.kubernetes.io/uninitialized\",\"value\":\"true\",\"effect\":\"NoSchedule\"}}]'"},
		{"install hcloud integration", fmt.Sprintf("kubectl -n kube-system create secret generic hcloud --from-literal=token=%s", AppConf.CurrentContext.Token)},
		{"deploy cloud controller manager", "kubectl apply -f  https://raw.githubusercontent.com/hetznercloud/hcloud-cloud-controller-manager/master/deploy/v1.0.0.yaml"},
	}
	for _, node := range cluster.Nodes {
		if node.IsMaster {
			if len(cluster.Nodes) == 1 {
				commands = append(commands, SSHCommand{"taint master", "kubectl taint nodes --all node-role.kubernetes.io/master-"})
			}

			for _, command := range commands {
				cluster.coordinator.AddEvent(node.Name, command.eventName)
				_, err := runCmd(node, command.command)
				if err != nil {
					return err
				}
			}

			cluster.coordinator.AddEvent(node.Name, "complete!")
			break
		}
	}

	return nil
}

func (cluster *Cluster) InstallWorkers() error {
	var joinCommand string
	// find master
	for _, node := range cluster.Nodes {
		if node.IsMaster {
			output, err := runCmd(node, "kubeadm token create --print-join-command")
			if err != nil {
				return err
			}
			joinCommand = output
			break
		}
	}

	// now let the nodes join

	for _, node := range cluster.Nodes {
		if !node.IsMaster {
			cluster.coordinator.AddEvent(node.Name, "registering node")
			_, err := runCmd(node, "swapoff -a && "+joinCommand)
			if err != nil {
				return err
			}

			cluster.coordinator.AddEvent(node.Name, "complete!")
		}
	}

	return nil
}

func randomName() string {
	return fmt.Sprintf("%s-%s%s", randomdata.Adjective(), randomdata.Noun(), randomdata.Adjective())
}

func validateClusterCreateFlags(cmd *cobra.Command, args []string) error {

	var (
		ssh_key, master_server_type, worker_server_type string
	)

	if ssh_key, _ = cmd.Flags().GetString("ssh-key"); ssh_key == "" {
		return errors.New("flag --ssh-key is required")
	}

	if master_server_type, _ = cmd.Flags().GetString("master-server-type"); master_server_type == "" {
		return errors.New("flag --master_server_type is required")
	}

	if worker_server_type, _ = cmd.Flags().GetString("worker-server-type"); worker_server_type == "" {
		return errors.New("flag --worker_server_type is required")
	}

	if index, _ := AppConf.Config.FindSSHKeyByName(ssh_key); index == -1 {
		return errors.New(fmt.Sprintf("SSH key '%s' not found", ssh_key))
	}

	return nil
}

func init() {
	clusterCmd.AddCommand(clusterCreateCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// clusterCreateCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	clusterCreateCmd.Flags().String("name", "", "Name of the cluster")
	clusterCreateCmd.Flags().String("ssh-key", "", "Name of the SSH key used for provisioning")
	clusterCreateCmd.Flags().String("master-server-type", "cx11", "Server type used of masters")
	clusterCreateCmd.Flags().String("worker-server-type", "cx11", "Server type used of workers")
	clusterCreateCmd.Flags().Bool("self-hosted", false, "If true, the kubernetes control plane will be hosted on itself")
	clusterCreateCmd.Flags().IntP("nodes", "n", 2, "Number of nodes for the cluster")
}
