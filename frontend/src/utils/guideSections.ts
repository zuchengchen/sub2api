import { readChapterTitle, type GuideChapter } from './guideMarkdown'

/**
 * The guide bundled with the release. Each chapter is its own file under
 * docs/guide/ named `NNN-slug.md`: the numeric prefix orders the chapter and
 * the rest is its permanent anchor slug. Editing a file and rebuilding is all
 * that is needed to change the default guide; admins override chapters at
 * runtime through the database instead.
 */
const chapterModules = import.meta.glob<string>('../../../docs/guide/*.md', {
  query: '?raw',
  import: 'default',
  eager: true,
})

const fileNamePattern = /(\d+)-([a-z0-9]+(?:-[a-z0-9]+)*)\.md$/

function parseBundledChapters(): GuideChapter[] {
  const parsed: Array<{ order: number; chapter: GuideChapter }> = []

  for (const [path, raw] of Object.entries(chapterModules)) {
    const match = fileNamePattern.exec(path)
    // A file that does not follow NNN-slug.md has no derivable anchor, so it is
    // skipped rather than rendered under an unstable generated id.
    if (!match) continue

    const content = raw.replace(/\s+$/, '')
    if (!content.trim()) continue

    parsed.push({
      order: Number(match[1]),
      chapter: {
        slug: match[2],
        title: readChapterTitle(content),
        content,
      },
    })
  }

  return parsed
    .sort((a, b) => a.order - b.order || a.chapter.slug.localeCompare(b.chapter.slug))
    .map((entry) => entry.chapter)
}

const bundledChapters = parseBundledChapters()

/** Returns a fresh copy so callers cannot mutate the bundled baseline. */
export function getBundledGuideChapters(): GuideChapter[] {
  return bundledChapters.map((chapter) => ({ ...chapter }))
}
