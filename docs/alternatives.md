# Choosing a tool

Choose based on the execution model you need, rather than a claim that one tool
replaces the others. The guidance below is a scope comparison, not a performance
benchmark.

| Tool | Useful when | Irrigator's focus |
| --- | --- | --- |
| Shell scripts / GNU Make | You have a predictable command sequence or file dependency build. [Make manual](https://www.gnu.org/software/make/manual/) | Agent attempts, accepted results, explicit uncertainty and deliberate recovery in a local run directory. |
| LangGraph | You want to develop agent behavior in a graph-based application framework with persistence and human intervention. [LangChain overview](https://www.langchain.com/oss-overview) | Operate explicit plans through a local CLI and host skills, with provider accounting and artifact acceptance. |
| Temporal | You need durable application workflows coordinated by a deployed service, either self-hosted or Temporal Cloud. [Deployment model](https://docs.temporal.io/production-deployment) | A single-host execution tool that keeps its run evidence on local disk. |
| Conductor OSS (originally Netflix Conductor) | You need an event-driven workflow server coordinating distributed workers and services, including AI agents. [conductor-oss/conductor](https://github.com/conductor-oss/conductor) | No server to deploy: a local CLI with provider accounting, artifact acceptance checks and explicit recovery of uncertain calls. |

Irrigator does not provide multi-host scheduling, an untrusted-code sandbox, or a
hosted control plane. For small one-off tasks, a direct model call or a script can
be simpler. Start with [the mock tutorial](first-run.md) to see whether persistent
agent task state is useful for your workflow.
