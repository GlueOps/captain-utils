module "captain" {
  iam_role_to_assume = "arn:aws:iam::000000000000:role/glueops-captain-role"
  source             = "git::https://github.com/GlueOps/terraform-module-cloud-aws-kubernetes-cluster.git?ref=v0.55.0"
  eks_version        = "1.33"
  csi_driver_version = "v1.45.0-eksbuild.2"
  coredns_version    = "v1.11.4-eksbuild.14"
  kube_proxy_version = "v1.33.0-eksbuild.2"
  vpc_cidr_block     = "10.65.0.0/26"
  region             = "us-west-2"
  availability_zones = ["us-west-2a", "us-west-2b", "us-west-2c"]
  node_pools = [
    #    {
    #      "ami_release_version" : "1.32.3-20250410",
    #      "ami_type" : "AL2023_x86_64_STANDARD",
    #      "instance_type" : "t3a.large",
    #      "kubernetes_version" : "1.32",
    #      "name" : "glueops-platform-node-pool-1",
    #      "node_count" : 2,
    #    },
    {
      "ami_release_version" : "1.33.0-20250519",
      "ami_type" : "AL2023_x86_64_STANDARD",
      "instance_type" : "t3a.large",
      "kubernetes_version" : "1.33",
      "name" : "glueops-platform-node-pool-2",
      "node_count" : 2,
      "spot" : false,
      "disk_size_gb" : 20,
      "max_pods" : 110,
      "ssh_key_pair_names" : [],
      "kubernetes_labels" : {
        "glueops.dev/role" : "glueops-platform"
      },
      "kubernetes_taints" : [
        {
          key    = "glueops.dev/role"
          value  = "glueops-platform"
          effect = "NO_SCHEDULE"
        }
      ]
    }
  ]

  peering_configs = [
  ]
}
