# OpenSandbox Ingress

本文记录沙箱入站流量转发相关的领域术语。

## Language

**Ingress 副本（Ingress replica）**：一个独立运行的 OpenSandbox 入站网关实例，接收客户端请求并将其转发给对应的沙箱服务。_Avoid_: 沙箱副本、集群网关总量

**沙箱上游（Sandbox upstream）**：Ingress 为某个沙箱请求选定的服务目标。同一个沙箱可以提供多个端口对应的不同服务目标。_Avoid_: 下游客户端、沙箱总数

**execd**：运行在沙箱中的执行服务，为客户端提供命令执行、文件操作等能力。_Avoid_: Ingress、生命周期服务
