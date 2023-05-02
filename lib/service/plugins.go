/*
Copyright 2023 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package service

import (
	"github.com/gravitational/trace"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/api/utils"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/srv/plugins"
)

func (process *TeleportProcess) shouldInitPlugins() bool {
	return process.Config.Plugins.Enabled && !process.Config.Plugins.IsEmpty()
}

func (process *TeleportProcess) initPlugins() {
	process.RegisterWithAuthServer(types.RolePlugins, PluginsIdentityEvent)
	process.RegisterCriticalFunc("plugins.init", process.initPluginsService)
}

func (process *TeleportProcess) initPluginsService() error {
	log := process.log.WithField(trace.Component, teleport.Component(
		teleport.ComponentPlugins, process.id))

	conn, err := process.WaitForConnector(PluginsIdentityEvent, log)
	if conn == nil {
		return trace.Wrap(err)
	}

	accessPoint, err := process.newLocalCacheForPlugins(conn.Client,
		[]string{teleport.ComponentPlugins})
	if err != nil {
		return trace.Wrap(err)
	}

	// asyncEmitter makes sure that sessions do not block
	// in case if connections are slow
	asyncEmitter, err := process.NewAsyncEmitter(conn.Client)
	if err != nil {
		return trace.Wrap(err)
	}

	pluginMatcher := types.Labels{}
	for k, v := range process.Config.Plugins.Plugins {
		pluginMatcher[k] = utils.Strings{v}
	}
	pluginsService, err := plugins.New(process.ExitContext(), &plugins.Config{
		APIClient:   conn.Client,
		Emitter:     asyncEmitter,
		Log:         process.log,
		AccessPoint: accessPoint,
		ResourceMatchers: []services.ResourceMatcher{
			{
				Labels: pluginMatcher,
			},
		},
	})
	if err != nil {
		return trace.Wrap(err)
	}

	process.OnExit("plugins.stop", func(payload interface{}) {
		log.Info("Shutting down.")
		if pluginsService != nil {
			pluginsService.Stop()
		}
		if asyncEmitter != nil {
			warnOnErr(asyncEmitter.Close(), process.log)
		}
		warnOnErr(conn.Close(), log)
		log.Info("Exited.")
	})

	process.BroadcastEvent(Event{Name: PluginsReady, Payload: nil})

	if err := pluginsService.Start(process.ExitContext()); err != nil {
		return trace.Wrap(err)
	}
	log.Infof("Plugins service has successfully started")

	if err := pluginsService.Wait(); err != nil {
		return trace.Wrap(err)
	}

	return nil
}
