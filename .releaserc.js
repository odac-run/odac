module.exports = {
  branches: ['main'],
  plugins: [
    [
      '@semantic-release/commit-analyzer',
      {
        preset: 'conventionalcommits'
      }
    ],
    [
      '@semantic-release/release-notes-generator',
      {
        preset: 'conventionalcommits',
        writerOpts: {
          transform: (c, context) => {
            const commit = JSON.parse(JSON.stringify(c))

            const map = {
              feat: "✨ What's New",
              fix: '🛠️ Fixes & Improvements',
              perf: '⚡️ Performance Upgrades',
              refactor: '⚙️ Engine Tuning',
              docs: '📚 Documentation',
              style: '🎨 Style',
              test: '✅ Tests',
              chore: '🔧 Maintenance & Cleanup',
              build: '🏗️ Build',
              ci: '🤖 CI'
            }

            commit.type = map[commit.type] || commit.type

            const hide = ['🎨 Style', '🔧 Maintenance & Cleanup', '🏗️ Build', '🤖 CI', '✅ Tests']
            if (!commit.type || hide.includes(commit.type)) return false

            if (commit.scope === '*' || commit.scope === 'root') commit.scope = ''

            if (commit.notes) {
              commit.notes.forEach(n => {
                n.title = '💥 BREAKING CHANGES'
              })
            }

            if (commit.subject) {
              // Find and extract PR number from the subject, e.g., "feat: new thing (#123)"
              const prRegex = /\s\(#(\d+)\)$/
              const prMatch = commit.subject.match(prRegex)
              const prNumber = prMatch ? prMatch[1] : null

              // If a PR number is found, remove it from the subject to avoid it appearing twice
              if (prNumber) {
                commit.subject = commit.subject.replace(prRegex, '')
              }

              let prLink = ''

              // Get author name - prefer GitHub login if available
              const email = commit.committer.email || commit.authorEmail || ''
              const ghUserMatch = email.match(/^(?:\d+\+)?([a-zA-Z0-9-]+)@users\.noreply\.github\.com$/)
              const ghUser = ghUserMatch ? ghUserMatch[1] : null

              let attribution = ''
              if (ghUser) attribution = `by @${ghUser}`

              // Get PR link if a number was found
              if (prNumber && context.host && context.owner && context.repository) {
                const prUrl = `https://${context.host}/${context.owner}/${context.repository}/pull/${prNumber}`
                const prLink = `[#${prNumber}](${prUrl})`
                attribution = attribution ? `${attribution} in ${prLink}` : prLink
              }

              // Append the attribution string to the subject if we created one
              // if (attribution && prLink) {
              //   commit.subject = `${commit.subject} ${attribution} in ${prLink}`;
              // } else if (prLink) {
              //   commit.subject = `${commit.subject} ${prLink}`;
              // } else if (attribution) {
              //   commit.subject = `${commit.subject} ${attribution}`;
              // }
            }
            return commit
          },
          groupBy: 'type',
          commitGroupsSort: (a, b) => (a.title > b.title ? 1 : -1),
          commitsSort: ['scope', 'subject'],
          headerPartial: '',
          commitPartial: '- {{#if scope}}**{{scope}}:** {{/if}}{{subject}}\n',
          mainTemplate: `
{{#if commitGroups}}
{{#each commitGroups}}
### {{title}}

{{#each commits}}
{{> commit root=@root}}
{{/each}}

{{/each}}
{{/if}}

{{#if noteGroups}}
{{#each noteGroups}}
### {{title}}

{{#each notes}}
- {{text}}
{{/each}}

{{/each}}
{{/if}}

---

Powered by [⚡ Odac](https://odac.run)
`
        }
      }
    ],
    [
      '@semantic-release/changelog',
      {
        changelogFile: 'CHANGELOG.md'
      }
    ],
    '@semantic-release/npm',
    [
      '@semantic-release/git',
      {
        assets: ['package.json', 'CHANGELOG.md'],
        message: '⚡ Odac v${nextRelease.version} Released'
      }
    ],
    '@semantic-release/github'
  ]
}
