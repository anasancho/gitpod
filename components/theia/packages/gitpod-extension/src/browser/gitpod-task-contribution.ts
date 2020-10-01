/**
 * Copyright (c) 2020 TypeFox GmbH. All rights reserved.
 * Licensed under the GNU Affero General Public License (AGPL).
 * See License-AGPL.txt in the project root for license information.
 */

import { injectable, inject, postConstruct } from 'inversify';
import { FrontendApplicationContribution, ApplicationShell } from '@theia/core/lib/browser';
import { TerminalFrontendContribution } from '@theia/terminal/lib/browser/terminal-frontend-contribution';
import { TerminalWidget } from '@theia/terminal/lib/browser/base/terminal-widget';
import { GitpodTerminalWidget } from './gitpod-terminal-widget';
import { IBaseTerminalServer } from '@theia/terminal/lib/common/base-terminal-protocol';

interface TaskStatus {
    alias: string
    name?: string
    openIn?: ApplicationShell.WidgetOptions['area']
    openMode?: ApplicationShell.WidgetOptions['mode']
}

interface GitpodTaskTerminalWidget extends GitpodTerminalWidget {
    readonly kind: 'gitpod-task'
    taskAlias?: string
}
namespace GitpodTaskTerminalWidget {
    const idPrefix = 'gitpod-task-terminal'
    export function is(terminal: TerminalWidget): terminal is GitpodTaskTerminalWidget {
        return terminal.kind === 'gitpod-task';
    }
    export function toId(index: number): string {
        return idPrefix + ':' + index;
    }
    export function getIndex(terminal: GitpodTaskTerminalWidget): number {
        const [, index] = terminal.id.split(':');
        return Number(index);
    }
}

@injectable()
export class GitpodTaskContribution implements FrontendApplicationContribution {

    @inject(TerminalFrontendContribution)
    protected readonly terminals: TerminalFrontendContribution;

    @postConstruct()
    protected init(): void {
        this.terminals.onDidCreateTerminal(terminal => {
            if (GitpodTaskTerminalWidget.is(terminal)) {
                terminal.onTerminalDidClose(() => {
                    if (terminal.taskAlias) {
                        fetch(window.location.protocol + '//' + window.location.host + '/_supervisor/v1/terminal/close/' + terminal.taskAlias)
                    } else {
                        console.error('task alias is missing');
                    }
                });
            }
        });
    }

    async onDidInitializeLayout() {
        // TODO monitor task status and close terminal widget if an underlying terminal exits
        const response = await fetch(window.location.protocol + '//' + window.location.host + '/_supervisor/v1/status/tasks')
        const status: {
            tasks: TaskStatus[]
        } = await response.json();

        const terminals = new Map<string, GitpodTaskTerminalWidget>();
        for (const terminal of this.terminals.all) {
            if (GitpodTaskTerminalWidget.is(terminal)) {
                terminals.set(terminal.id, terminal);
            }
        }
        const toClose = new Set(terminals.keys());

        let ref: TerminalWidget | undefined;
        for (let index = 0; index < status.tasks.length; index++) {
            const task = status.tasks[index];
            try {
                const id = GitpodTaskTerminalWidget.toId(index);
                toClose.delete(id);
                let terminal = terminals.get(id);
                if (!terminal) {
                    terminal = await this.terminals.newTerminal({
                        id,
                        kind: 'gitpod-task',
                        title: task.name,
                        useServerTitle: false
                    }) as GitpodTaskTerminalWidget;
                    terminal.taskAlias = task.alias;
                    await terminal.start();
                    await terminal.executeCommand({
                        cwd: '/workspace',
                        args: `/theia/supervisor terminal attach ${terminal.taskAlias} -ir`.split(' ')
                    });
                    this.terminals.activateTerminal(terminal, {
                        ref,
                        area: task.openIn || 'bottom',
                        mode: task.openMode || 'tab-after'
                    });
                } else if (!IBaseTerminalServer.validateId(terminal.terminalId)) {
                    // reuse terminal
                    terminal.taskAlias = task.alias;
                    await terminal.start();
                    await terminal.executeCommand({
                        cwd: '/workspace',
                        args: `/theia/supervisor terminal attach ${terminal.taskAlias} -ir`.split(' ')
                    });
                }
                ref = terminal;
            } catch (e) {
                console.error('Failed to start Gitpod task terminal:', e);
            }
        }

        for (const id of toClose) {
            const terminal = terminals.get(id);
            if (terminal) {
                terminal.dispose();
            }
        }

        // if there is no terminal at all, lets start one
        if (!this.terminals.all.length) {
            const terminal = await this.terminals.newTerminal({});
            terminal.start();
            this.terminals.open(terminal);
        }
    }

}