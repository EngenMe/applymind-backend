package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsapigateway"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscertificatemanager"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsevents"
	"github.com/aws/aws-cdk-go/awscdk/v2/awseventstargets"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslambda"
	"github.com/aws/aws-cdk-go/awscdk/v2/awslogs"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsroute53"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsroute53targets"
	"github.com/aws/aws-cdk-go/awscdk/v2/awss3"
	"github.com/aws/constructs-go/constructs/v10"
	"github.com/aws/jsii-runtime-go"
)

const (
	// Existing hosted zone for faroukhasnaoui.tech — confirmed present via
	// `aws route53 list-hosted-zones-by-name`. We add a record to it; we do
	// NOT create a new zone or touch delegation.
	hostedZoneID   = "Z03831462JGL0H5K6RYHX"
	hostedZoneName = "faroukhasnaoui.tech"

	// Relative record name within the zone above, so the full custom domain
	// is api.applymind.faroukhasnaoui.tech.
	apiRecordName = "api.applymind"

	logRetentionDays = awslogs.RetentionDays_ONE_MONTH // matches diagram: 30 days
)

func NewApplymindStack(scope constructs.Construct, id string, props *awscdk.StackProps) awscdk.Stack {
	stack := awscdk.NewStack(scope, jsii.String(id), props)

	apiDomainName := apiRecordName + "." + hostedZoneName

	// ---------------------------------------------------------------------
	// Required application config, read once at synth time from .env (same
	// file the Makefile already loads). These become literal values baked
	// into the synthesized template — see DEPLOY-CHECKLIST.md for why
	// infra/cdk.out must stay out of git.
	// ---------------------------------------------------------------------
	neonDatabaseURL := requireEnv("NEON_DATABASE_URL")
	apiKey := requireEnv("APPLYMIND_API_KEY")
	openAIKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) // optional, config.go treats it as such
	openAIModel := strings.TrimSpace(os.Getenv("OPENAI_MODEL")) // optional
	corsOrigins := strings.TrimSpace(os.Getenv("CORS_ALLOWED_ORIGINS"))
	if corsOrigins == "" {
		corsOrigins = "http://localhost:3000"
	}

	// ---------------------------------------------------------------------
	// S3 bucket for CV / cover-letter storage (prefixes cvs/, cover-letters/
	// are applied by the application, not the bucket itself).
	// ---------------------------------------------------------------------
	bucket := awss3.NewBucket(
		stack, jsii.String("FilesBucket"), &awss3.BucketProps{
			BucketName: jsii.String(
				fmt.Sprintf(
					"applymind-files-%s-%s",
					*stack.Account(), *stack.Region(),
				),
			),
			Versioned:         jsii.Bool(true),
			Encryption:        awss3.BucketEncryption_S3_MANAGED,
			BlockPublicAccess: awss3.BlockPublicAccess_BLOCK_ALL(),
			// Needed for the direct browser presigned GET/PUT path shown in the
			// diagram. Restricted to the same origins the API's CORS middleware
			// allows, so this list needs the same post-dashboard-deploy update.
			Cors: &[]*awss3.CorsRule{
				{
					AllowedMethods: &[]awss3.HttpMethods{
						awss3.HttpMethods_GET,
						awss3.HttpMethods_PUT,
					},
					AllowedOrigins: splitOrigins(corsOrigins),
					AllowedHeaders: jsii.Strings("*"),
					MaxAge:         jsii.Number(3000),
				},
			},
			// RETAIN, not DESTROY: this bucket holds the user's actual CVs and
			// cover letters. `cdk destroy` must not be able to take them out.
			RemovalPolicy: awscdk.RemovalPolicy_RETAIN,
		},
	)

	// ---------------------------------------------------------------------
	// Lambda 1: applymind-api
	// ---------------------------------------------------------------------
	apiLogGroup := awslogs.NewLogGroup(
		stack, jsii.String("ApiLogGroup"), &awslogs.LogGroupProps{
			LogGroupName:  jsii.String("/aws/lambda/applymind-api"),
			Retention:     logRetentionDays,
			RemovalPolicy: awscdk.RemovalPolicy_DESTROY, // logs, not user data
		},
	)

	apiRole := awsiam.NewRole(
		stack, jsii.String("ApiFunctionRole"), &awsiam.RoleProps{
			AssumedBy: awsiam.NewServicePrincipal(jsii.String("lambda.amazonaws.com"), nil),
			ManagedPolicies: &[]awsiam.IManagedPolicy{
				awsiam.ManagedPolicy_FromAwsManagedPolicyName(jsii.String("service-role/AWSLambdaBasicExecutionRole")),
				awsiam.ManagedPolicy_FromAwsManagedPolicyName(jsii.String("AWSXRayDaemonWriteAccess")),
			},
		},
	)

	apiEnv := map[string]*string{
		"NEON_DATABASE_URL":    jsii.String(neonDatabaseURL),
		"APPLYMIND_API_KEY":    jsii.String(apiKey),
		"APPLYMIND_CV_BUCKET":  bucket.BucketName(),
		"CORS_ALLOWED_ORIGINS": jsii.String(corsOrigins),
	}
	if openAIKey != "" {
		apiEnv["OPENAI_API_KEY"] = jsii.String(openAIKey)
	}
	if openAIModel != "" {
		apiEnv["OPENAI_MODEL"] = jsii.String(openAIModel)
	}

	apiFunction := awslambda.NewFunction(
		stack, jsii.String("ApiFunction"), &awslambda.FunctionProps{
			FunctionName: jsii.String("applymind-api"),
			Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
			Architecture: awslambda.Architecture_ARM_64(),
			Handler:      jsii.String("bootstrap"),
			Code:         awslambda.Code_FromAsset(jsii.String("lambda/api"), nil),
			MemorySize:   jsii.Number(256),
			// API Gateway's own integration timeout is a hard 29s, so anything
			// higher here can never actually be reached on this path.
			Timeout:   awscdk.Duration_Seconds(jsii.Number(29)),
			Tracing:   awslambda.Tracing_ACTIVE,
			Role:      apiRole,
			LogGroup:  apiLogGroup,
			Environment: &apiEnv,
		},
	)

	// S3 read/write, scoped to this bucket only — nothing broader.
	bucket.GrantReadWrite(apiFunction, nil)

	// ---------------------------------------------------------------------
	// API Gateway (REST) in front of Lambda 1, all routes proxied through
	// Chi inside the single function.
	// ---------------------------------------------------------------------
	zone := awsroute53.HostedZone_FromHostedZoneAttributes(
		stack, jsii.String("Zone"), &awsroute53.HostedZoneAttributes{
			HostedZoneId: jsii.String(hostedZoneID),
			ZoneName:     jsii.String(hostedZoneName),
		},
	)

	cert := awscertificatemanager.NewCertificate(
		stack, jsii.String("ApiCertificate"), &awscertificatemanager.CertificateProps{
			DomainName:  jsii.String(apiDomainName),
			Validation:  awscertificatemanager.CertificateValidation_FromDns(zone),
		},
	)

	api := awsapigateway.NewLambdaRestApi(
		stack, jsii.String("Api"), &awsapigateway.LambdaRestApiProps{
			Handler: apiFunction,
			Proxy:   jsii.Bool(true),
			EndpointConfiguration: &awsapigateway.EndpointConfiguration{
				Types: &[]awsapigateway.EndpointType{awsapigateway.EndpointType_REGIONAL},
			},
			DeployOptions: &awsapigateway.StageOptions{
				StageName:      jsii.String("prod"),
				TracingEnabled: jsii.Bool(true),
			},
			DomainName: &awsapigateway.DomainNameOptions{
				DomainName:   jsii.String(apiDomainName),
				Certificate:  cert,
				EndpointType: awsapigateway.EndpointType_REGIONAL,
			},
		},
	)

	awsroute53.NewARecord(
		stack, jsii.String("ApiAliasRecord"), &awsroute53.ARecordProps{
			Zone:       zone,
			RecordName: jsii.String(apiRecordName),
			Target: awsroute53.RecordTarget_FromAlias(
				awsroute53targets.NewApiGatewayDomain(api.DomainName()),
			),
		},
	)

	// ---------------------------------------------------------------------
	// Lambda 2: applymind-scheduler, invoked by EventBridge on a daily cron.
	// No S3 access: cmd/scheduler never constructs an S3 client.
	// ---------------------------------------------------------------------
	schedulerLogGroup := awslogs.NewLogGroup(
		stack, jsii.String("SchedulerLogGroup"), &awslogs.LogGroupProps{
			LogGroupName:  jsii.String("/aws/lambda/applymind-scheduler"),
			Retention:     logRetentionDays,
			RemovalPolicy: awscdk.RemovalPolicy_DESTROY,
		},
	)

	schedulerRole := awsiam.NewRole(
		stack, jsii.String("SchedulerFunctionRole"), &awsiam.RoleProps{
			AssumedBy: awsiam.NewServicePrincipal(jsii.String("lambda.amazonaws.com"), nil),
			ManagedPolicies: &[]awsiam.IManagedPolicy{
				awsiam.ManagedPolicy_FromAwsManagedPolicyName(jsii.String("service-role/AWSLambdaBasicExecutionRole")),
				awsiam.ManagedPolicy_FromAwsManagedPolicyName(jsii.String("AWSXRayDaemonWriteAccess")),
			},
		},
	)

	schedulerFunction := awslambda.NewFunction(
		stack, jsii.String("SchedulerFunction"), &awslambda.FunctionProps{
			FunctionName: jsii.String("applymind-scheduler"),
			Runtime:      awslambda.Runtime_PROVIDED_AL2023(),
			Architecture: awslambda.Architecture_ARM_64(),
			Handler:      jsii.String("bootstrap"),
			Code:         awslambda.Code_FromAsset(jsii.String("lambda/scheduler"), nil),
			MemorySize:   jsii.Number(256),
			Timeout:      awscdk.Duration_Seconds(jsii.Number(60)),
			Tracing:      awslambda.Tracing_ACTIVE,
			Role:         schedulerRole,
			LogGroup:     schedulerLogGroup,
			Environment: &map[string]*string{
				"NEON_DATABASE_URL": jsii.String(neonDatabaseURL),
			},
		},
	)

	rule := awsevents.NewRule(
		stack, jsii.String("DailyReminderRule"), &awsevents.RuleProps{
			RuleName: jsii.String("applymind-daily-reminder"),
			Schedule: awsevents.Schedule_Expression(jsii.String("cron(0 8 * * ? *)")),
		},
	)
	rule.AddTarget(awseventstargets.NewLambdaFunction(schedulerFunction, nil))

	// ---------------------------------------------------------------------
	// Outputs
	// ---------------------------------------------------------------------
	awscdk.NewCfnOutput(
		stack, jsii.String("ApiDefaultUrl"), &awscdk.CfnOutputProps{
			Value:       api.Url(),
			Description: jsii.String("Default execute-api base URL (always works, no DNS propagation wait)"),
		},
	)
	awscdk.NewCfnOutput(
		stack, jsii.String("ApiCustomDomainUrl"), &awscdk.CfnOutputProps{
			Value:       jsii.String("https://" + apiDomainName + "/"),
			Description: jsii.String("Custom domain URL — usable once the ACM cert validates and DNS propagates"),
		},
	)
	awscdk.NewCfnOutput(
		stack, jsii.String("FilesBucketName"), &awscdk.CfnOutputProps{
			Value: bucket.BucketName(),
		},
	)

	return stack
}

// requireEnv panics with a clear message rather than deploying a Lambda with
// an empty required config value. Fails synth, not the running function.
func requireEnv(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		panic(fmt.Sprintf("infra: required env var %s is not set (check your .env)", key))
	}
	return v
}

// splitOrigins turns a comma-separated origins string into the *[]*string
// shape the S3 CORS rule needs, trimming blanks the same way config.go's
// parseOrigins does for the API's own CORS middleware.
func splitOrigins(raw string) *[]*string {
	parts := strings.Split(raw, ",")
	out := make([]*string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, jsii.String(trimmed))
		}
	}
	return &out
}
