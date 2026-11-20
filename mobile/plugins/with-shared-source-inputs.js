const { withAppBuildGradle } = require('expo/config-plugins')

// Metro watches the shared projector and interaction hooks, but Gradle's
// default bundle inputs stop at the mobile project root. Keep those external
// sources in the incremental-build fingerprint after every Expo prebuild.
const marker = '// Go Bot shared frontend bundle inputs'
module.exports = config => withAppBuildGradle(config, next => {
  if (!next.modResults.contents.includes(marker)) {
    next.modResults.contents += `
${marker}
tasks.matching { it.name.startsWith("createBundle") && it.name.endsWith("JsAndAssets") }.configureEach {
    inputs.dir(new File(projectRoot, "../frontend/src"))
        .withPropertyName("sharedFrontendSource")
        .withPathSensitivity(org.gradle.api.tasks.PathSensitivity.RELATIVE)
}
`
  }
  return next
})
