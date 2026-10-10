import json
import sys
from collections import Counter

REPORT = "license-report.json"

# These are the artifact types we consider actual third-party packages.
PACKAGE_TYPES = {
    "npm",
    "python",
    "golang",
    "java",
    "rust",
    "ruby",
    "php",
    "dotnet",
    "swift",
    "dart",
    "apk",
    "deb",
    "rpm",
    "gem",
    "cocoapods",
    "conan",
    "hex",
    "hackage",
    "erlang-hex",
}

# Licenses that are acceptable by default.
APPROVED_LICENSES = {
    "MIT",
    "Apache-2.0",
    "BSD-2-Clause",
    "BSD-3-Clause",
    "ISC",
    "PSF-2.0",
}

# Licenses that need security/legal review.
REVIEW_LICENSES = {
    "LGPL-3.0-only",
    "LGPL-3.0-or-later",
    "MPL-2.0",
    "BlueOak-1.0.0",
    "CC0-1.0",
    "CC-BY-SA-4.0",
    "BSL-1.0",
    "Unlicense",
    "Python-2.0",
    "BSD",
    "Dual License",
}


def normalize_license(value):
    """
    Normalize common license strings reported by Syft.
    """

    if not value:
        return None

    value = str(value).strip()

    # Remove surrounding parentheses.
    while value.startswith("(") and value.endswith(")"):
        value = value[1:-1].strip()

    # Syft sometimes returns SHA256 values as license data.
    if value.lower().startswith("sha256:"):
        return "INVALID_LICENSE_DATA"

    # Exact aliases.
    aliases = {
        "MIT License": "MIT",
        "Apache 2": "Apache-2.0",
        "Apache 2.0": "Apache-2.0",
        "Apache License Version 2.0": "Apache-2.0",
        "Apache License, Version 2.0": "Apache-2.0",
        "ISC License": "ISC",
        "PSF": "PSF-2.0",
        "3-Clause BSD License": "BSD-3-Clause",
    }

    if value in aliases:
        return aliases[value]

    # Handle longer Apache license text.
    if "Apache License Version 2.0" in value:
        return "Apache-2.0"

    if "Apache License, Version 2.0" in value:
        return "Apache-2.0"

    return value


def evaluate_license(value):
    """
    Return:
      APPROVED
      REVIEW_REQUIRED
      LICENSE_NOT_DETECTED
      INVALID_LICENSE_DATA
    """

    if not value:
        return "LICENSE_NOT_DETECTED"

    value = normalize_license(value)

    if value == "INVALID_LICENSE_DATA":
        return "INVALID_LICENSE_DATA"

    # Handle SPDX OR expressions.
    #
    # Example:
    # Apache-2.0 OR BSD-2-Clause
    #
    # Both are approved -> APPROVED
    #
    # MIT OR CC0-1.0
    #
    # CC0 requires review -> REVIEW_REQUIRED
    if " OR " in value:
        options = [
            normalize_license(option.strip())
            for option in value.split(" OR ")
        ]

        results = [
            evaluate_license(option)
            for option in options
        ]

        if all(result == "APPROVED" for result in results):
            return "APPROVED"

        return "REVIEW_REQUIRED"

    if value in APPROVED_LICENSES:
        return "APPROVED"

    if value in REVIEW_LICENSES:
        return "REVIEW_REQUIRED"

    # Unknown licenses are not automatically approved.
    return "REVIEW_REQUIRED"


def main():

    try:
        with open(REPORT, "r", encoding="utf-8") as f:
            data = json.load(f)

    except FileNotFoundError:
        print(f"ERROR: {REPORT} not found")
        sys.exit(1)

    artifacts = data.get("artifacts", [])

    results = []

    summary = Counter()

    skipped_types = Counter()

    package_artifact_count = 0
    skipped_artifact_count = 0

    for artifact in artifacts:

        package = artifact.get("name", "UNKNOWN")
        version = artifact.get("version", "UNKNOWN")
        artifact_type = artifact.get("type", "UNKNOWN")

        # Ignore repository files, directories, GitHub Actions metadata,
        # configuration files, etc.
        #
        # We only evaluate actual third-party package artifacts.
        if artifact_type not in PACKAGE_TYPES:

            skipped_artifact_count += 1
            skipped_types[artifact_type] += 1

            continue

        package_artifact_count += 1

        licenses = artifact.get("licenses") or []

        # Package has no detected license.
        if not licenses:

            status = "LICENSE_NOT_DETECTED"

            results.append(
                {
                    "package": package,
                    "version": version,
                    "type": artifact_type,
                    "license": None,
                    "normalized_license": None,
                    "status": status,
                }
            )

            summary[status] += 1

            continue

        # Evaluate every license record.
        for license_info in licenses:

            if isinstance(license_info, dict):

                license_value = (
                    license_info.get("spdxExpression")
                    or license_info.get("value")
                    or ""
                )

            else:

                license_value = str(license_info)

            normalized = normalize_license(license_value)

            status = evaluate_license(license_value)

            results.append(
                {
                    "package": package,
                    "version": version,
                    "type": artifact_type,
                    "license": license_value,
                    "normalized_license": normalized,
                    "status": status,
                }
            )

            summary[status] += 1

    # Create machine-readable policy report.
    output_file = "license-policy-report.json"

    with open(output_file, "w", encoding="utf-8") as f:

        json.dump(
            {
                "summary": {
                    "total_syft_artifacts": len(artifacts),
                    "package_artifacts_evaluated": package_artifact_count,
                    "non_package_artifacts_skipped": skipped_artifact_count,
                    "approved": summary["APPROVED"],
                    "review_required": summary["REVIEW_REQUIRED"],
                    "license_not_detected": summary[
                        "LICENSE_NOT_DETECTED"
                    ],
                    "invalid_license_data": summary[
                        "INVALID_LICENSE_DATA"
                    ],
                },
                "skipped_artifact_types": dict(skipped_types),
                "results": results,
            },
            f,
            indent=2,
        )

    # Console summary.
    print("")
    print("==============================================")
    print("       OpenSandbox License Policy")
    print("==============================================")

    print(f"Total Syft artifacts       : {len(artifacts)}")
    print(
        f"Package artifacts evaluated: "
        f"{package_artifact_count}"
    )
    print(
        f"Non-package artifacts skipped: "
        f"{skipped_artifact_count}"
    )

    print("----------------------------------------------")

    print(
        f"APPROVED                   : "
        f"{summary['APPROVED']}"
    )

    print(
        f"REVIEW_REQUIRED            : "
        f"{summary['REVIEW_REQUIRED']}"
    )

    print(
        f"LICENSE_NOT_DETECTED       : "
        f"{summary['LICENSE_NOT_DETECTED']}"
    )

    print(
        f"INVALID_LICENSE_DATA       : "
        f"{summary['INVALID_LICENSE_DATA']}"
    )

    print("==============================================")

    # Show review candidates.
    print("")
    print("Review candidates:")
    print("----------------------------------------------")

    review_count = 0

    for result in results:

        if result["status"] in {
            "REVIEW_REQUIRED",
            "LICENSE_NOT_DETECTED",
            "INVALID_LICENSE_DATA",
        }:

            print(
                f"{result['package']} "
                f"{result['version']} "
                f"[{result['type']}] "
                f"-> {result['license']} "
                f"[{result['status']}]"
            )

            review_count += 1

            if review_count >= 50:

                print("... showing first 50 candidates")
                break

    print("")

    # IMPORTANT:
    #
    # We currently do NOT fail the pipeline for review candidates.
    # This is intentional for the MVP because human/legal review
    # is expected before final approval.
    #
    # Later we can introduce:
    #
    # PASS
    # WARNING
    # FAIL
    #
    # based on company policy.

    sys.exit(0)


if __name__ == "__main__":
    main()
