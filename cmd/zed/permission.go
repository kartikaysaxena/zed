package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/authzed/authzed-go/pkg/requestmeta"
	"github.com/authzed/authzed-go/pkg/responsemeta"
	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/authzed/authzed-go/v1"
	"github.com/cockroachdb/cockroach/pkg/util/treeprinter"
	"github.com/jzelinskie/cobrautil"
	"github.com/jzelinskie/stringz"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/authzed/zed/internal/printers"
	"github.com/authzed/zed/internal/storage"
)

func registerPermissionCmd(rootCmd *cobra.Command) {
	rootCmd.AddCommand(permissionCmd)

	permissionCmd.AddCommand(checkCmd)
	checkCmd.Flags().Bool("json", false, "output as JSON")
	checkCmd.Flags().String("revision", "", "optional revision at which to check")
	checkCmd.Flags().Bool("explain", false, "requests debug information from SpiceDB and prints out a trace of the requests")
	checkCmd.Flags().Bool("schema", false, "requests debug information from SpiceDB and prints out the schema used")
	checkCmd.Flags().String("caveat-context", "", "the caveat context to send along with the check, in JSON form")

	permissionCmd.AddCommand(expandCmd)
	expandCmd.Flags().Bool("json", false, "output as JSON")
	expandCmd.Flags().String("revision", "", "optional revision at which to check")

	permissionCmd.AddCommand(lookupCmd)
	lookupCmd.Flags().Bool("json", false, "output as JSON")
	lookupCmd.Flags().String("revision", "", "optional revision at which to check")
	lookupCmd.Flags().String("caveat-context", "", "the caveat context to send along with the lookup, in JSON form")

	permissionCmd.AddCommand(lookupResourcesCmd)
	lookupResourcesCmd.Flags().Bool("json", false, "output as JSON")
	lookupResourcesCmd.Flags().String("revision", "", "optional revision at which to check")
	lookupResourcesCmd.Flags().String("caveat-context", "", "the caveat context to send along with the lookup, in JSON form")

	permissionCmd.AddCommand(lookupSubjectsCmd)
	lookupSubjectsCmd.Flags().Bool("json", false, "output as JSON")
	lookupSubjectsCmd.Flags().String("revision", "", "optional revision at which to check")
	lookupSubjectsCmd.Flags().String("caveat-context", "", "the caveat context to send along with the lookup, in JSON form")
}

var permissionCmd = &cobra.Command{
	Use:   "permission <subcommand>",
	Short: "perform queries on the Permissions in a Permissions System",
}

var checkCmd = &cobra.Command{
	Use:   "check <resource:id> <permission> <subject:id>",
	Short: "check that a Permission exists for a Subject",
	Args:  cobra.ExactArgs(3),
	RunE:  cobrautil.CommandStack(LogCmdFunc, checkCmdFunc),
}

var expandCmd = &cobra.Command{
	Use:   "expand <permission> <resource:id>",
	Short: "expand the structure of a Permission",
	Args:  cobra.ExactArgs(2),
	RunE:  cobrautil.CommandStack(LogCmdFunc, expandCmdFunc),
}

var lookupResourcesCmd = &cobra.Command{
	Use:   "lookup-resources <type> <permission> <subject:id>",
	Short: "looks up the Resources of a given type for which the Subject has Permission",
	Args:  cobra.ExactArgs(3),
	RunE:  cobrautil.CommandStack(LogCmdFunc, lookupResourcesCmdFunc),
}

var lookupCmd = &cobra.Command{
	Use:    "lookup <type> <permission> <subject:id>",
	Short:  "lookup the Resources of a given type for which the Subject has Permission",
	Args:   cobra.ExactArgs(3),
	RunE:   cobrautil.CommandStack(LogCmdFunc, lookupResourcesCmdFunc),
	Hidden: true,
}

var lookupSubjectsCmd = &cobra.Command{
	Use:   "lookup-subjects <resource:id> <permission> <subject_type#optional_subject_relation>",
	Short: "lookup the Subjects of a given type for which the Subject has Permission on the Resource",
	Args:  cobra.ExactArgs(3),
	RunE:  cobrautil.CommandStack(LogCmdFunc, lookupSubjectsCmdFunc),
}

func parseSubject(s string) (namespace, id, relation string, err error) {
	err = stringz.SplitExact(s, ":", &namespace, &id)
	if err != nil {
		return
	}
	err = stringz.SplitExact(id, "#", &id, &relation)
	if err != nil {
		relation = ""
		err = nil
	}
	return
}

func parseType(s string) (namespace, relation string) {
	namespace, relation, _ = strings.Cut(s, "#")
	return
}

func getCaveatContext(cmd *cobra.Command) (*structpb.Struct, error) {
	contextString := cobrautil.MustGetString(cmd, "caveat-context")
	if len(contextString) == 0 {
		return nil, nil
	}

	return parseCaveatContext(contextString)
}

func checkCmdFunc(cmd *cobra.Command, args []string) error {
	var objectNS, objectID string
	err := stringz.SplitExact(args[0], ":", &objectNS, &objectID)
	if err != nil {
		return err
	}

	relation := args[1]

	subjectNS, subjectID, subjectRel, err := parseSubject(args[2])
	if err != nil {
		return err
	}

	caveatContext, err := getCaveatContext(cmd)
	if err != nil {
		return err
	}

	configStore, secretStore := defaultStorage()
	token, err := storage.DefaultToken(
		cobrautil.MustGetString(cmd, "endpoint"),
		cobrautil.MustGetString(cmd, "token"),
		configStore,
		secretStore,
	)
	if err != nil {
		return err
	}
	log.Trace().Interface("token", token).Send()

	client, err := authzed.NewClient(token.Endpoint, dialOptsFromFlags(cmd, token)...)
	if err != nil {
		return err
	}

	request := &v1.CheckPermissionRequest{
		Resource: &v1.ObjectReference{
			ObjectType: objectNS,
			ObjectId:   objectID,
		},
		Permission: relation,
		Subject: &v1.SubjectReference{
			Object: &v1.ObjectReference{
				ObjectType: subjectNS,
				ObjectId:   subjectID,
			},
			OptionalRelation: subjectRel,
		},
		Context: caveatContext,
	}

	if zedtoken := cobrautil.MustGetString(cmd, "revision"); zedtoken != "" {
		request.Consistency = atLeastAsFresh(zedtoken)
	}
	log.Trace().Interface("request", request).Send()

	ctx := context.Background()
	if cobrautil.MustGetBool(cmd, "explain") || cobrautil.MustGetBool(cmd, "schema") {
		log.Info().Msg("debugging requested on check")
		ctx = requestmeta.AddRequestHeaders(ctx, requestmeta.RequestDebugInformation)
	}

	var trailerMD metadata.MD
	resp, err := client.CheckPermission(ctx, request, grpc.Trailer(&trailerMD))
	if err != nil {
		derr := displayDebugInformationIfRequested(cmd, trailerMD, true)
		if derr != nil {
			return derr
		}

		return err
	}

	if cobrautil.MustGetBool(cmd, "json") {
		prettyProto, err := prettyProto(resp)
		if err != nil {
			return err
		}

		fmt.Println(string(prettyProto))
		return nil
	}

	switch resp.Permissionship {
	case v1.CheckPermissionResponse_PERMISSIONSHIP_CONDITIONAL_PERMISSION:
		log.Warn().Strs("fields", resp.PartialCaveatInfo.MissingRequiredContext).Msg("missing fields in caveat context")
		fmt.Println("caveated")

	case v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION:
		fmt.Println("true")

	case v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION:
		fmt.Println("false")

	default:
		return fmt.Errorf("unknown permission response: %v", resp.Permissionship)
	}

	return displayDebugInformationIfRequested(cmd, trailerMD, false)
}

func expandCmdFunc(cmd *cobra.Command, args []string) error {
	relation := args[0]

	var objectNS, objectID string
	err := stringz.SplitExact(args[1], ":", &objectNS, &objectID)
	if err != nil {
		return err
	}

	configStore, secretStore := defaultStorage()
	token, err := storage.DefaultToken(
		cobrautil.MustGetString(cmd, "endpoint"),
		cobrautil.MustGetString(cmd, "token"),
		configStore,
		secretStore,
	)
	if err != nil {
		return err
	}
	log.Trace().Interface("token", token).Send()

	client, err := authzed.NewClient(token.Endpoint, dialOptsFromFlags(cmd, token)...)
	if err != nil {
		return err
	}

	request := &v1.ExpandPermissionTreeRequest{
		Resource: &v1.ObjectReference{
			ObjectType: objectNS,
			ObjectId:   objectID,
		},
		Permission: relation,
	}

	if zedtoken := cobrautil.MustGetString(cmd, "revision"); zedtoken != "" {
		request.Consistency = atLeastAsFresh(zedtoken)
	}
	log.Trace().Interface("request", request).Send()

	resp, err := client.ExpandPermissionTree(context.Background(), request)
	if err != nil {
		return err
	}

	if cobrautil.MustGetBool(cmd, "json") {
		prettyProto, err := prettyProto(resp)
		if err != nil {
			return err
		}

		fmt.Println(string(prettyProto))
		return nil
	}

	tp := treeprinter.New()
	printers.TreeNodeTree(tp, resp.TreeRoot)
	fmt.Println(tp.String())

	return nil
}

func lookupResourcesCmdFunc(cmd *cobra.Command, args []string) error {
	objectNS := args[0]
	relation := args[1]
	subjectNS, subjectID, subjectRel, err := parseSubject(args[2])
	if err != nil {
		return err
	}

	caveatContext, err := getCaveatContext(cmd)
	if err != nil {
		return err
	}

	configStore, secretStore := defaultStorage()
	token, err := storage.DefaultToken(
		cobrautil.MustGetString(cmd, "endpoint"),
		cobrautil.MustGetString(cmd, "token"),
		configStore,
		secretStore,
	)
	if err != nil {
		return err
	}
	log.Trace().Interface("token", token).Send()

	client, err := authzed.NewClient(token.Endpoint, dialOptsFromFlags(cmd, token)...)
	if err != nil {
		return err
	}

	request := &v1.LookupResourcesRequest{
		ResourceObjectType: objectNS,
		Permission:         relation,
		Subject: &v1.SubjectReference{
			Object: &v1.ObjectReference{
				ObjectType: subjectNS,
				ObjectId:   subjectID,
			},
			OptionalRelation: subjectRel,
		},
		Context: caveatContext,
	}

	if zedtoken := cobrautil.MustGetString(cmd, "revision"); zedtoken != "" {
		request.Consistency = atLeastAsFresh(zedtoken)
	}
	log.Trace().Interface("request", request).Send()

	respStream, err := client.LookupResources(context.Background(), request)
	if err != nil {
		return err
	}

	for {
		resp, err := respStream.Recv()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		default:
			if cobrautil.MustGetBool(cmd, "json") {
				prettyProto, err := prettyProto(resp)
				if err != nil {
					return err
				}

				fmt.Println(string(prettyProto))
			}

			fmt.Println(prettyLookupPermissionship(resp.ResourceObjectId, resp.Permissionship, resp.PartialCaveatInfo))
		}
	}
}

func lookupSubjectsCmdFunc(cmd *cobra.Command, args []string) error {
	var objectNS, objectID string
	err := stringz.SplitExact(args[0], ":", &objectNS, &objectID)
	if err != nil {
		return err
	}

	permission := args[1]

	subjectType, subjectRelation := parseType(args[2])

	caveatContext, err := getCaveatContext(cmd)
	if err != nil {
		return err
	}

	configStore, secretStore := defaultStorage()
	token, err := storage.DefaultToken(
		cobrautil.MustGetString(cmd, "endpoint"),
		cobrautil.MustGetString(cmd, "token"),
		configStore,
		secretStore,
	)
	if err != nil {
		return err
	}
	log.Trace().Interface("token", token).Send()

	client, err := authzed.NewClient(token.Endpoint, dialOptsFromFlags(cmd, token)...)
	if err != nil {
		return err
	}

	request := &v1.LookupSubjectsRequest{
		Resource: &v1.ObjectReference{
			ObjectType: objectNS,
			ObjectId:   objectID,
		},
		Permission:              permission,
		SubjectObjectType:       subjectType,
		OptionalSubjectRelation: subjectRelation,
		Context:                 caveatContext,
	}

	if zedtoken := cobrautil.MustGetString(cmd, "revision"); zedtoken != "" {
		request.Consistency = atLeastAsFresh(zedtoken)
	}
	log.Trace().Interface("request", request).Send()

	respStream, err := client.LookupSubjects(context.Background(), request)
	if err != nil {
		return err
	}

	for {
		resp, err := respStream.Recv()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		default:
			if cobrautil.MustGetBool(cmd, "json") {
				prettyProto, err := prettyProto(resp)
				if err != nil {
					return err
				}

				fmt.Println(string(prettyProto))
			}
			fmt.Printf("%s:%s%s\n",
				subjectType,
				prettyLookupPermissionship(resp.Subject.SubjectObjectId, resp.Subject.Permissionship, resp.Subject.PartialCaveatInfo),
				excludedSubjectsString(resp.ExcludedSubjects),
			)
		}
	}
}

func excludedSubjectsString(excluded []*v1.ResolvedSubject) string {
	if len(excluded) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, " - {\n")
	for _, subj := range excluded {
		fmt.Fprintf(&b, "\t%s\n", prettyLookupPermissionship(
			subj.SubjectObjectId,
			subj.Permissionship,
			subj.PartialCaveatInfo,
		))
	}
	fmt.Fprintf(&b, "}")
	return b.String()
}

func prettyLookupPermissionship(objectID string, p v1.LookupPermissionship, info *v1.PartialCaveatInfo) string {
	var b strings.Builder
	fmt.Fprint(&b, objectID)
	if p == v1.LookupPermissionship_LOOKUP_PERMISSIONSHIP_CONDITIONAL_PERMISSION {
		fmt.Fprintf(&b, " (caveated, missing context: %s)", strings.Join(info.MissingRequiredContext, ", "))
	}
	return b.String()
}

func displayDebugInformationIfRequested(cmd *cobra.Command, trailerMD metadata.MD, hasError bool) error {
	if cobrautil.MustGetBool(cmd, "explain") || cobrautil.MustGetBool(cmd, "schema") {
		found, err := responsemeta.GetResponseTrailerMetadataOrNil(trailerMD, responsemeta.DebugInformation)
		if err != nil {
			return err
		}

		if found == nil {
			log.Warn().Msg("No debuging information returned for the check")
			return nil
		}

		debugInfo := &v1.DebugInformation{}
		err = protojson.Unmarshal([]byte(*found), debugInfo)
		if err != nil {
			return err
		}

		if debugInfo.Check == nil {
			log.Warn().Msg("No trace found for the check")
			return nil
		}

		if cobrautil.MustGetBool(cmd, "explain") {
			tp := treeprinter.New()
			printers.DisplayCheckTrace(debugInfo.Check, tp, hasError)
			fmt.Println()
			fmt.Println(tp.String())
		}

		if cobrautil.MustGetBool(cmd, "schema") {
			fmt.Println()
			fmt.Println(debugInfo.SchemaUsed)
		}
	}
	return nil
}
