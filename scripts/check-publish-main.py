#!/usr/bin/env python3
"""Run the workflow's actual gate against disposable local Git repositories."""
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap

ROOT = Path(__file__).resolve().parent.parent
workflow = (ROOT / '.github/workflows/publish-main.yml').read_text()
step = workflow.split('      - name: Require CI to have passed for this commit\n', 1)[1]
script = textwrap.dedent(step.split('        run: |\n', 1)[1].split('\n      - name:', 1)[0])


def git(cwd, *args):
    return subprocess.check_output(['git', '-C', str(cwd), *args], text=True, stderr=subprocess.DEVNULL).strip()


with tempfile.TemporaryDirectory() as directory:
    root = Path(directory)
    remote = root / 'remote'
    work = root / 'work'
    git(root, 'init', '--bare', '--initial-branch=main', str(remote))
    git(root, 'clone', str(remote), str(work))
    git(work, 'config', 'user.name', 'Gate Test')
    git(work, 'config', 'user.email', 'gate@example.invalid')
    git(work, 'commit', '--allow-empty', '-m', 'tested')
    tested = git(work, 'rev-parse', 'HEAD')
    git(work, 'push', 'origin', 'main')
    # Only the hosted checks API is stubbed; Git resolves a real remote tip.
    bin_dir = root / 'bin'
    bin_dir.mkdir()
    gh = bin_dir / 'gh'
    gh.write_text('#!/bin/sh\nfor job in ci-backend-api ci-backend-other ci-frontend ci-relay ci-scripts ci-docker; do\n  printf "%s\\tcompleted\\t%s\\n" "$job" "${CHECK_RESULT:-success}"\ndone\n')
    gh.chmod(0o755)
    output = root / 'output'
    env = dict(os.environ, PATH=str(bin_dir) + os.pathsep + os.environ['PATH'],
               TESTED_SHA=tested, GITHUB_OUTPUT=str(output), REPO='test/repo')

    def check(name, expected, **overrides):
        output.write_text('')
        result = subprocess.run(['bash', '-c', script], cwd=work,
                                env=dict(env, **overrides), text=True, capture_output=True)
        actual = output.read_text().splitlines()
        if expected is None:
            assert result.returncode != 0 and 'publish=true' not in actual, (name, result, actual)
        else:
            assert result.returncode == 0 and actual == [f'publish={expected}'], (name, result, actual)
        print(f'PASS: {name}')

    check('current tested main', 'true')
    check('checkout differs from tested commit', 'false', TESTED_SHA='0' * 40)
    check('failed required CI check', None, CHECK_RESULT='failure')
    git(work, 'commit', '--allow-empty', '-m', 'new main')
    git(work, 'push', 'origin', 'main')
    git(work, 'checkout', '--detach', tested)
    check('rerun of old checkout after remote main advances', 'false')
    git(work, 'remote', 'set-url', 'origin', str(root / 'missing'))
    check('remote lookup fails closed', None)
