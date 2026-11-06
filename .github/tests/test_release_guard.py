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
        "group": "release-fork-images", "cancel-in-progress": "false"}
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


def test_latest_promotion_contract():
    workflow = load(WORKFLOWS / "release.yml")
    steps = workflow["jobs"]["release"]["steps"]
    decision = next(i for i, step in enumerate(steps) if step.get("id") == "latest")
    build = next(i for i, step in enumerate(steps) if step.get("id") == "build")
    assert decision < build
    assert steps[decision]["run"] == "python3 .github/scripts/latest_release.py"
    assert "steps.latest.outputs.latest == 'true'" in steps[build]["with"]["tags"]


def test_latest_version_ordering():
    import importlib.util
    spec = importlib.util.spec_from_file_location("latest_release", ROOT / ".github/scripts/latest_release.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)

    def refs(*tags):
        return "\n".join(f"{'a' * 40}\trefs/tags/{tag}" for tag in tags)

    assert module.promote("v0.17.2.vcluster.0", refs("v0.16.5.vcluster.1", "v0.17.2.vcluster.0"))
    assert not module.promote("v0.16.5.vcluster.1", refs("v0.16.5.vcluster.1", "v0.17.2.vcluster.0"))
    assert not module.promote("v0.17.2.vcluster.0", refs("v0.17.2.vcluster.0", "v0.17.2.vcluster.1"))
    assert module.promote("v0.17.2.vcluster.1", refs("v0.17.2.vcluster.1", "v0.18.0.vcluster.0-rc.1"))
    assert not module.promote("v0.18.0.vcluster.0-rc.1", refs("v0.18.0.vcluster.0-rc.1"))
    import pytest
    with pytest.raises(ValueError):
        module.promote("v0.17.2.vcluster.0", refs("v0.16.5.vcluster.1"))


def test_remote_lookup_failure_stops_promotion(monkeypatch, tmp_path):
    import importlib.util
    import subprocess
    import pytest
    spec = importlib.util.spec_from_file_location("latest_release", ROOT / ".github/scripts/latest_release.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    output = tmp_path / "output"
    monkeypatch.setenv("TAG", "v0.17.2.vcluster.0")
    monkeypatch.setenv("GITHUB_OUTPUT", str(output))

    def fail(*args, **kwargs):
        raise subprocess.CalledProcessError(1, "git ls-remote")

    monkeypatch.setattr(module.subprocess, "run", fail)
    with pytest.raises(subprocess.CalledProcessError):
        module.main()
    assert not output.exists()
