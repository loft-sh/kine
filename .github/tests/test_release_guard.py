"""Release publishing invariants for the fork tooling snapshot."""
from pathlib import Path
import yaml

ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github" / "workflows"
GUARD = "loft-sh/github-actions/.github/actions/ghcr-tag-guard@ghcr-tag-guard/v1"


def load(path):
    return yaml.load(path.read_text(), Loader=yaml.BaseLoader)


def test_publisher_and_guard_contract():
    assert not (WORKFLOWS / "release.yaml").exists()
    publishers = []
    for path in list(WORKFLOWS.glob("*.yml")) + list(WORKFLOWS.glob("*.yaml")):
        workflow = load(path)
        for job in workflow.get("jobs", {}).values():
            steps = job.get("steps", [])
            pushes = [(i, step) for i, step in enumerate(steps)
                      if step.get("with", {}).get("push") == "true"]
            if pushes:
                publishers.append((path, workflow, job, steps, pushes))
    assert len(publishers) == 1
    path, workflow, job, steps, pushes = publishers[0]
    assert path.name == "release.yml"
    assert workflow["on"] == {"push": {"tags": ["v*.vcluster.*"]}}
    assert workflow["concurrency"] == {
        "group": "release-${{ github.ref_name }}", "cancel-in-progress": "false"}
    guards = [(i, step) for i, step in enumerate(steps) if step.get("uses") == GUARD]
    assert len(guards) == 1
    index, guard = guards[0]
    assert index == 0, "guard must precede all job steps and any possible writes"
    immutable = set()
    for push_index, step in pushes:
        assert index < push_index
        for tag in step["with"]["tags"].splitlines():
            if "${{ github.ref_name }}" in tag:
                immutable.add(tag.strip())
    assert immutable
    assert set(guard["with"]["images"].splitlines()) == immutable
