// VulcanizeDB
// Copyright © 2019 Vulcanize

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.

// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"fmt"
	"plugin"
	syn "sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/vulcanize/vulcanizedb/libraries/shared/constants"
	"github.com/vulcanize/vulcanizedb/libraries/shared/fetcher"
	"github.com/vulcanize/vulcanizedb/libraries/shared/storage"
	"github.com/vulcanize/vulcanizedb/libraries/shared/streamer"
	"github.com/vulcanize/vulcanizedb/libraries/shared/transformer"
	"github.com/vulcanize/vulcanizedb/libraries/shared/watcher"
	"github.com/vulcanize/vulcanizedb/pkg/fs"
	"github.com/vulcanize/vulcanizedb/utils"
)

// executeCmd represents the execute command
var executeCmd = &cobra.Command{
	Use:   "execute",
	Short: "executes a precomposed transformer initializer plugin",
	Long: `This command needs a config .toml file of form:
[database]
    name     = "vulcanize_public"
    hostname = "localhost"
    user     = "vulcanize"
    password = "vulcanize"
    port     = 5432
[client]
    ipcPath  = "/Users/user/Library/Ethereum/geth.ipc"
[exporter]
    name     = "exampleTransformerExporter"
Note: If any of the plugin transformer need additional
configuration variables include them in the .toml file as well
The exporter.name is the name (without extension) of the plugin to be loaded.
The plugin file needs to be located in the /plugins directory and this command assumes 
the db migrations remain from when the plugin was composed. Additionally, the plugin 
must have been composed by the same version of vulcanizedb or else it will not be compatible.
Specify config location when executing the command:
./vulcanizedb execute --config=./environments/config_name.toml`,
	Run: func(cmd *cobra.Command, args []string) {
		subCommand = cmd.CalledAs()
		logWithCommand = *log.WithField("SubCommand", subCommand)
		execute()
	},
}

func execute() {
	// Build plugin generator config
	prepConfig()

	// Get the plugin path and load the plugin
	_, pluginPath, err := genConfig.GetPluginPaths()
	if err != nil {
		logWithCommand.Fatal(err)
	}

	fmt.Printf("Executing plugin %s", pluginPath)
	logWithCommand.Info("linking plugin ", pluginPath)
	plug, err := plugin.Open(pluginPath)
	if err != nil {
		logWithCommand.Warn("linking plugin failed")
		logWithCommand.Fatal(err)
	}

	// Load the `Exporter` symbol from the plugin
	logWithCommand.Info("loading transformers from plugin")
	symExporter, err := plug.Lookup("Exporter")
	if err != nil {
		logWithCommand.Warn("loading Exporter symbol failed")
		logWithCommand.Fatal(err)
	}

	// Assert that the symbol is of type Exporter
	exporter, ok := symExporter.(Exporter)
	if !ok {
		logWithCommand.Fatal("plugged-in symbol not of type Exporter")
	}

	// Use the Exporters export method to load the EventTransformerInitializer, StorageTransformerInitializer, and ContractTransformerInitializer sets
	ethEventInitializers, ethStorageInitializers, ethContractInitializers := exporter.Export()

	// Setup bc and db objects
	blockChain := getBlockChain()
	db := utils.LoadPostgres(databaseConfig, blockChain.Node())

	// Execute over transformer sets returned by the exporter
	// Use WaitGroup to wait on both goroutines
	var wg syn.WaitGroup
	if len(ethEventInitializers) > 0 {
		ew := watcher.NewEventWatcher(&db, blockChain)
		err = ew.AddTransformers(ethEventInitializers)
		if err != nil {
			logWithCommand.Fatalf("failed to add event transformer initializers to watcher: %s", err.Error())
		}
		wg.Add(1)
		go watchEthEvents(&ew, &wg)
	}

	if len(ethStorageInitializers) > 0 {
		switch storageDiffsSource {
		case "geth":
			log.Debug("fetching storage diffs from geth pub sub")
			wsClient := getWSClient()
			stateDiffStreamer := streamer.NewStateDiffStreamer(wsClient)
			storageFetcher := fetcher.NewGethRPCStorageFetcher(stateDiffStreamer)
			sw := watcher.NewStorageWatcher(storageFetcher, &db)
			sw.AddTransformers(ethStorageInitializers)
			wg.Add(1)
			go watchEthStorage(sw, &wg)
		default:
			log.Debug("fetching storage diffs from csv")
			tailer := fs.FileTailer{Path: storageDiffsPath}
			storageFetcher := fetcher.NewCsvTailStorageFetcher(tailer)
			sw := watcher.NewStorageWatcher(storageFetcher, &db)
			sw.AddTransformers(ethStorageInitializers)
			wg.Add(1)
			go watchEthStorage(sw, &wg)
		}
	}

	if len(ethContractInitializers) > 0 {
		gw := watcher.NewContractWatcher(&db, blockChain)
		gw.AddTransformers(ethContractInitializers)
		wg.Add(1)
		go watchEthContract(&gw, &wg)
	}
	wg.Wait()
}

func init() {
	rootCmd.AddCommand(executeCmd)
	executeCmd.Flags().BoolVarP(&recheckHeadersArg, "recheck-headers", "r", false, "whether to re-check headers for watched events")
	executeCmd.Flags().DurationVarP(&queueRecheckInterval, "queue-recheck-interval", "q", 5*time.Minute, "interval duration for rechecking queued storage diffs (ex: 5m30s)")
}

type Exporter interface {
	Export() ([]transformer.EventTransformerInitializer, []transformer.StorageTransformerInitializer, []transformer.ContractTransformerInitializer)
}

func watchEthEvents(w *watcher.EventWatcher, wg *syn.WaitGroup) {
	defer wg.Done()
	// Execute over the EventTransformerInitializer set using the watcher
	logWithCommand.Info("executing event transformers")
	var recheck constants.TransformerExecution
	if recheckHeadersArg {
		recheck = constants.HeaderRecheck
	} else {
		recheck = constants.HeaderUnchecked
	}
	err := w.Execute(recheck)
	if err != nil {
		logWithCommand.Fatalf("error executing event watcher: %s", err.Error())
	}
}

func watchEthStorage(w watcher.IStorageWatcher, wg *syn.WaitGroup) {
	defer wg.Done()
	// Execute over the StorageTransformerInitializer set using the storage watcher
	logWithCommand.Info("executing storage transformers")
	on := viper.GetBool("storageBackFill.on")
	if on {
		backFillStorage(w)
	}
	w.Execute(queueRecheckInterval, on)
}

func backFillStorage(w watcher.IStorageWatcher) {
	rpcClient, _ := getClients()
	// find min deployment block
	minDeploymentBlock := constants.GetMinDeploymentBlock()
	stateDiffFetcher := fetcher.NewStateDiffFetcher(rpcClient)
	backFiller := storage.NewStorageBackFiller(stateDiffFetcher, storage.DefaultMaxBatchSize)
	go w.BackFill(minDeploymentBlock, backFiller)
}

func watchEthContract(w *watcher.ContractWatcher, wg *syn.WaitGroup) {
	defer wg.Done()
	// Execute over the ContractTransformerInitializer set using the contract watcher
	logWithCommand.Info("executing contract_watcher transformers")
	ticker := time.NewTicker(pollingInterval)
	defer ticker.Stop()
	for range ticker.C {
		w.Execute()
	}
}
