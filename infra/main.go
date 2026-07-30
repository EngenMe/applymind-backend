// Command applymind-infra is the CDK app for Phase 8: it provisions the
// applymind-api and applymind-scheduler Lambdas, the API Gateway REST API
// (with a custom domain), the S3 files bucket, the daily EventBridge rule,
// and the supporting IAM/CloudWatch/X-Ray resources described in
// applymind-aws-architecture-mvp.drawio.
//
// This is infra only — no application code lives here. It builds two
// pre-compiled Lambda zips (see `make build-lambdas`) and wires AWS resources
// around them.
package main

import (
	"os"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/jsii-runtime-go"
	"github.com/joho/godotenv"
)

func main() {
	defer jsii.Close()

	// The Makefile already does `include .env; export` for local dev commands.
	// Here we load it directly so `cdk synth`/`cdk deploy`, run straight from
	// infra/, pick up the same values without needing the Makefile in between.
	// Missing file is fine under CI, where real env vars are expected to be
	// exported already.
	_ = godotenv.Load("../.env")
	_ = godotenv.Load(".env")

	app := awscdk.NewApp(nil)

	NewApplymindStack(app, "ApplymindBackendStack", &awscdk.StackProps{
		Env: cdkEnv(),
		Description: jsii.String(
			"ApplyMind MVP backend: API Gateway + 2 Lambdas + S3 + EventBridge (Phase 8)",
		),
	})

	app.Synth(nil)
}

// cdkEnv pins the deploy target to eu-west-1 per the architecture diagram,
// using whichever AWS account your CLI credentials resolve to. The CDK CLI
// sets CDK_DEFAULT_ACCOUNT/CDK_DEFAULT_REGION automatically from your
// credentials before invoking this binary.
func cdkEnv() *awscdk.Environment {
	account := os.Getenv("CDK_DEFAULT_ACCOUNT")
	return &awscdk.Environment{
		Account: jsii.String(account),
		Region:  jsii.String("eu-west-1"),
	}
}
