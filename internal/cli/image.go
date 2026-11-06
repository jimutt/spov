package cli

import (
	"fmt"
	"github.com/jimutt/spov/internal/config"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jimutt/spov/internal/sandbox"
	"github.com/jimutt/spov/internal/server"
	"github.com/jimutt/spov/internal/store"
	"github.com/jimutt/spov/internal/toolchain"
)

// newImageCmd is `spov serve image`.
//
// @decision(DL-032)
func newImageCmd(env *Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Build the sandbox base image",
		Long: "The base image every sandbox boots from. Its name carries the pinned OpenCode version, " +
			"so each pinned version needs its image built once before any session can use it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newImageBuildCmd(env), newImagePruneCmd(env))
	return cmd
}

// newImageBuildCmd is `spov serve image build`, the step DL-035 calls a
// deliberate release step and DL-032 makes reachable.
//
// @decision(DL-032) @decision(DL-035)
func newImageBuildCmd(env *Env) *cobra.Command {
	var version string
	var addons, apt []string
	var rebuild bool
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build and publish the sandbox base image for the pinned OpenCode version",
		Long: "Builds the image and publishes it under the name a session will ask for, then exits.\n\n" +
			"Building is separate from launching on purpose: a session never builds the base image, so " +
			"an unbuilt version fails the launch rather than silently spending minutes on a build nobody " +
			"asked for. Only an add-on variant is built on its first launch.\n\n" +
			"Already published? This returns immediately, so it is safe to run before every deploy.\n\n" +
			"--opencode-version builds a version other than the configured one, which is how a new pin " +
			"is prepared before anything is switched over to it.\n\n" +
			"--addons and --apt build an add-on variant ahead of its first launch, for a server that " +
			"does not build variants on demand (modal.build_on_demand: false).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := env.config()
			if err != nil {
				return err
			}
			want := cfg.Config.OpenCode.Version
			if version != "" {
				want = version
			}
			if want == "" {
				return fmt.Errorf("no OpenCode version configured; pass --opencode-version")
			}

			provider, err := env.provider(cmd.Context())
			if err != nil {
				return err
			}
			builder, ok := provider.(sandbox.ImageBuilder)
			if !ok {
				return fmt.Errorf("this sandbox provider cannot build images")
			}

			// @decision(DL-033) @decision(DL-016)
			sel, err := toolchain.Parse(addons)
			if err != nil {
				return err
			}
			spec := sandbox.ImageSpec{OpenCodeVersion: want, Addons: sel, ExtraPackages: apt}
			if err := spec.Validate(); err != nil {
				return err
			}
			warn := func(msg string) { _, _ = fmt.Fprintln(env.Err, "spov serve image build: "+msg) }
			st, closeStore, _, err := env.openStore(cmd.Context(), false, config.Config{}, warn)
			if err != nil {
				return err
			}
			defer closeStore()
			reg, file := env.imageRegistry(st, warn)
			// The exact build of what the add-ons resolve to today, the
			// image a launch looks for first.
			//
			// @decision(DL-033) @decision(DL-016)
			if len(sel) > 0 {
				spec.Addons = env.addonResolver(file, warn).Resolve(cmd.Context(), sel)
			}
			// Named before the work, because a build is minutes long
			// and the name is what a launch will look for — so a
			// mismatch is visible without waiting for it.
			//
			// "resolving", not "building": EnsureImage resolves first
			// and builds only on a miss, so an already-published image
			// returns in about a second. Announcing a build that did
			// not happen would make the safe-before-every-deploy case
			// read as a rebuild every time.
			_, _ = fmt.Fprintf(env.Out, "resolving %s\n", spec.Name())
			if len(sel) > 0 {
				_, _ = fmt.Fprintf(env.Out, "  add-ons: %s\n", strings.Join(toolchain.Installed(spec.Addons), ", "))
			}
			_, _ = fmt.Fprintln(env.Out, "  (already published? this returns at once; otherwise it builds, which takes minutes)")

			// A name pruning deleted still resolves, so it is built
			// again without looking it up.
			//
			// @decision(DL-016)
			ensure := builder.EnsureImage
			if rebuild {
				ensure = builder.BuildImage
			} else if reg != nil {
				if img, err := reg.Get(cmd.Context(), spec.Name()); err == nil && img.DeletedAt != nil {
					ensure = builder.BuildImage
				}
			}
			name, err := ensure(cmd.Context(), spec)
			if err != nil {
				return err
			}
			if reg != nil {
				img := server.VariantImage(spec)
				if len(spec.Addons) == 0 && len(sandbox.ExtraPackages(spec.ExtraPackages)) == 0 {
					img = store.Image{Name: name, Kind: store.ImageBase}
				}
				img.PublishedAt = env.now()
				if id, err := builder.ImageID(cmd.Context(), name); err == nil {
					img.ID = id
				}
				if err := reg.Record(cmd.Context(), img); err != nil {
					warn(fmt.Sprintf("record image %s: %v", name, err))
				}
			}
			_, _ = fmt.Fprintf(env.Out, "published %s\n", name)
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "opencode-version", "",
		"build this version instead of the configured one")
	cmd.Flags().StringSliceVar(&addons, "addons", nil,
		"build the variant with these add-ons, as name or name@version ("+strings.Join(toolchain.Names(), ", ")+")")
	cmd.Flags().StringSliceVar(&apt, "apt", nil, "build the variant with these extra Debian packages")
	cmd.Flags().BoolVar(&rebuild, "rebuild", false,
		"build and publish again even though the name resolves, for an image deleted outside this deployment's registry")
	return cmd
}

// newImagePruneCmd is `spov serve image prune`.
//
// @decision(DL-016)
func newImagePruneCmd(env *Env) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete published images nothing has used for a while",
		Long: "Deletes the images in the deployment's image registry that have gone unused: a variant a newer build " +
			"replaced after 7 days, the newest build of a request after 30 days (only while variants are built on demand), " +
			"the base image of an older pin after 30 days and an older proxy image after 7 days. The current base and " +
			"proxy images, and any image a live session was created from, are kept.\n\n" +
			"The server runs this once a day by itself; --dry-run lists what it would delete. Images published before " +
			"the registry existed are not in it and are never deleted here.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := env.config()
			if err != nil {
				return err
			}
			provider, err := env.provider(cmd.Context())
			if err != nil {
				return err
			}
			builder, ok := provider.(sandbox.ImageBuilder)
			if !ok {
				return fmt.Errorf("this sandbox provider cannot delete images")
			}
			warn := func(msg string) { _, _ = fmt.Fprintln(env.Err, "spov serve image prune: "+msg) }
			st, closeStore, _, err := env.openStore(cmd.Context(), false, config.Config{}, warn)
			if err != nil {
				return err
			}
			defer closeStore()
			reg, _ := env.imageRegistry(st, warn)
			if reg == nil {
				return fmt.Errorf("there is no image registry to prune from")
			}
			// A local server's sessions live in its own memory, out of
			// reach here; every retention outlasts a sandbox's 24-hour
			// limit, so their images were used too recently to go.
			live, err := st.Sessions().ListLive(cmd.Context())
			if err != nil {
				return err
			}
			inUse := map[string]bool{}
			for _, sess := range live {
				inUse[sess.Booted.Image], inUse[sess.Request.Image] = true, true
			}
			policy := server.PrunePolicy{
				Now:           env.now(),
				CurrentBase:   sandbox.ImageSpec{OpenCodeVersion: cfg.Config.OpenCode.Version}.Name(),
				InUse:         inUse,
				BuildOnDemand: cfg.Config.Modal.BuildOnDemand,
			}
			if bin := env.proxyBinary(); len(bin) > 0 {
				policy.CurrentProxy = sandbox.ProxyImageName(bin)
			}
			done, err := server.PruneImages(cmd.Context(), reg, builder, policy, dryRun)
			verb := "deleted"
			if dryRun {
				verb = "would delete"
			}
			for _, c := range done {
				_, _ = fmt.Fprintf(env.Out, "%s %s (%s)\n", verb, c.Image.Name, c.Why)
			}
			if len(done) == 0 && err == nil {
				_, _ = fmt.Fprintln(env.Out, "nothing to prune")
			}
			if !dryRun {
				if serr := reg.SetPrunedAt(cmd.Context(), policy.Now); serr != nil {
					warn(serr.Error())
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list what would be deleted, and delete nothing")
	return cmd
}
