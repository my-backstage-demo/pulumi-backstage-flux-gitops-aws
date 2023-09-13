# How to build an internal developer platform on Kubernetes with Backstage, IaC, and GitOps

## Introduction

This guide will show you how to build an internal developer platform on Kubernetes with Backstage, IaC, and GitOps. It
will take you through the steps of setting up a Backstage instance, configuring it to use IaC, and then deploying it to
Kubernetes with GitOps.

The code consists of two Pulumi programs, one for the Backstage instance and one for the IaC:

- [gitops-infra](/gitops-infra) - Pulumi program for the IaC
- [backstage-infra](/backstage-infra) - Pulumi program for the Backstage instance
